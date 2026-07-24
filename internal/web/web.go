package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/objectstore"
	"github.com/zachlatta/tasks/internal/task"
)

// Reader provides the fixed task projection the index page renders. In
// production it is the PostgreSQL store; tests use an in-memory implementation.
type Reader interface {
	Tasks(ctx context.Context) ([]task.Task, error)
}

// SessionStore persists browser sessions keyed by a hash of the session cookie
// value, so the raw cookie is never stored at rest. In production it is the
// PostgreSQL store; New falls back to an in-memory store when none is supplied
// (used by tests).
type SessionStore interface {
	SaveSession(ctx context.Context, tokenHash, csrf string, expiresAt time.Time) error
	Session(ctx context.Context, tokenHash string) (csrf string, expiresAt time.Time, ok bool, err error)
	DeleteSession(ctx context.Context, tokenHash string) error
}

const (
	sessionCookie = "tasks_session"
	// sessionCookieMaxAge bounds how long a browser remembers the login cookie.
	// Server-side sessions never expire, so this only limits how long a client
	// can be away before the shared secret must be re-entered. Browsers cap
	// persistent cookies near 400 days regardless of a larger value, and
	// requireSession slides the cookie forward on every page load, so an active
	// client stays signed in indefinitely.
	sessionCookieMaxAge = 400 * 24 * time.Hour
	maxAttachmentSize   = 50 << 20
	excerptLimit        = 180
)

//go:embed templates/*.html static/*.css static/*.js
var assets embed.FS

var markdownRenderer = goldmark.New(goldmark.WithExtensions(extension.GFM))

type Config struct {
	Tasks         *task.Service
	Reader        Reader
	Objects       objectstore.Store
	Auth          *auth.Server
	SecureCookies bool
	Now           func() time.Time
	// Sessions persists browser sessions. When nil, an in-memory store is used
	// (non-durable; intended for tests and single-process use).
	Sessions SessionStore
}

type handler struct {
	tasks         *task.Service
	reader        Reader
	objects       objectstore.Store
	auth          *auth.Server
	secureCookies bool
	now           func() time.Time
	templates     *template.Template
	mux           *http.ServeMux
	sessions      SessionStore
}

type session struct {
	CSRF      string
	ExpiresAt time.Time
}

type sessionContextKey struct{}

type pageData struct {
	Error      string
	CSRF       string
	Columns    []boardColumn
	DetailTask taskCard
	TaskCount  int
	Message    string
}

// boardColumn is one kanban column: a workflow state plus the cards currently
// parked in it, top card first.
type boardColumn struct {
	Status Status
	Label  string
	Empty  string
	Tasks  []taskCard
}

// Status is the workflow state a column or card belongs to. It mirrors
// task.Status and exists so templates can compare against a plain string.
type Status = task.Status

// taskCard is one task prepared for rendering: the stored task plus the
// preview text, dependency detail, and controls the board needs.
type taskCard struct {
	task.Task
	CSRF        string
	StatusLabel string
	Excerpt     string
	Relative    string
	Timestamp   string
	Blocked     bool
	DependsOn   []dependencyView
	Cover       *task.Attachment
	Moves       []moveOption
}

// dependencyView names a prerequisite so a card can show what is holding it up
// without the reader looking up opaque IDs.
type dependencyView struct {
	ID          string
	Title       string
	Status      Status
	StatusLabel string
	Done        bool
}

// moveOption is one column a card can be dropped into, rendered as a button for
// people who are not dragging.
type moveOption struct {
	Status Status
	Label  string
}

// moveResult is the JSON the board's drag and drop reads back, including the
// re-rendered card so the browser never has to duplicate card markup.
type moveResult struct {
	ID          string `json:"id"`
	Status      Status `json:"status"`
	StatusLabel string `json:"status_label"`
	Card        string `json:"card"`
	Message     string `json:"message"`
}

func New(config Config) http.Handler {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Sessions == nil {
		config.Sessions = newMemorySessionStore()
	}
	h := &handler{
		tasks:         config.Tasks,
		reader:        config.Reader,
		objects:       config.Objects,
		auth:          config.Auth,
		secureCookies: config.SecureCookies,
		now:           config.Now,
		mux:           http.NewServeMux(),
		sessions:      config.Sessions,
	}
	h.templates = template.Must(template.New("").Funcs(template.FuncMap{
		"renderMarkdown": renderMarkdown,
		"isImage":        isImage,
	}).ParseFS(assets, "templates/*.html"))
	h.mux.HandleFunc("GET /static/{file}", h.static)
	h.mux.HandleFunc("GET /login", h.loginPage)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.Handle("GET /{$}", h.requireSession(http.HandlerFunc(h.index)))
	h.mux.Handle("POST /logout", h.requireSession(http.HandlerFunc(h.logout)))
	h.mux.Handle("POST /tasks", h.requireSession(http.HandlerFunc(h.createTask)))
	h.mux.Handle("POST /tasks/{id}/move", h.requireSession(http.HandlerFunc(h.moveTask)))
	h.mux.Handle("POST /tasks/{id}/attachments", h.requireSession(http.HandlerFunc(h.uploadAttachment)))
	h.mux.Handle("GET /attachments/{key...}", h.requireSession(http.HandlerFunc(h.attachment)))
	// Keep the image routes working for pages loaded before attachments were
	// generalized and for existing links.
	h.mux.Handle("POST /tasks/{id}/images", h.requireSession(http.HandlerFunc(h.uploadAttachment)))
	h.mux.Handle("GET /images/{key...}", h.requireSession(http.HandlerFunc(h.attachment)))
	h.mux.Handle("GET /{id}", h.requireSession(http.HandlerFunc(h.task)))
	return securityHeaders(h.mux)
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contents, err := assets.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(name) {
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(contents)
}

func (h *handler) loginPage(w http.ResponseWriter, _ *http.Request) {
	h.render(w, http.StatusOK, "login.html", pageData{})
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !h.auth.CheckSecret(r.PostForm.Get("secret")) {
		h.render(w, http.StatusUnauthorized, "login.html", pageData{Error: "That secret code is not valid."})
		return
	}
	token := rand.Text()
	csrf := rand.Text()
	// A zero expiry marks the session as non-expiring: authenticated clients stay
	// signed in until they log out.
	if err := h.sessions.SaveSession(r.Context(), hashToken(token), csrf, time.Time{}); err != nil {
		h.render(w, http.StatusInternalServerError, "login.html", pageData{Error: "Could not start a session. Please try again."})
		return
	}
	h.setSessionCookie(w, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_ = h.sessions.DeleteSession(r.Context(), hashToken(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: h.secureCookies, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// setSessionCookie writes the persistent session cookie for token. It is used
// both when a session is created and to slide the cookie forward on page loads,
// so an active client is never dropped by the browser's cookie-lifetime cap.
func (h *handler) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  h.now().Add(sessionCookieMaxAge),
		MaxAge:   int(sessionCookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	items, err := h.reader.Tasks(r.Context())
	if err != nil {
		http.Error(w, "query tasks", http.StatusInternalServerError)
		return
	}
	current := r.Context().Value(sessionContextKey{}).(session)
	byID := make(map[string]task.Task, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	lookup := func(id string) (task.Task, bool) {
		found, ok := byID[id]
		return found, ok
	}
	columns := []boardColumn{
		{Status: task.StatusTodo, Label: "To do", Empty: "Nothing queued up."},
		{Status: task.StatusInProgress, Label: "In progress", Empty: "Nothing in motion."},
		{Status: task.StatusDone, Label: "Done", Empty: "Finished work lands here."},
	}
	position := map[Status]int{task.StatusTodo: 0, task.StatusInProgress: 1, task.StatusDone: 2}
	for _, item := range items {
		index, ok := position[item.Status]
		if !ok {
			index = 0
		}
		columns[index].Tasks = append(columns[index].Tasks, h.newTaskCard(item, current.CSRF, lookup))
	}
	h.render(w, http.StatusOK, "index.html", pageData{
		CSRF:      current.CSRF,
		Columns:   columns,
		TaskCount: len(items),
		Message:   r.URL.Query().Get("message"),
	})
}

func (h *handler) task(w http.ResponseWriter, r *http.Request) {
	item, err := h.tasks.Get(r.Context(), r.PathValue("id"))
	if errors.Is(err, task.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "query task", http.StatusInternalServerError)
		return
	}
	current := r.Context().Value(sessionContextKey{}).(session)
	card := h.newTaskCard(item, current.CSRF, h.storedTask(r.Context()))
	// The board fetches the same detail as a fragment for its slide-over panel.
	if r.URL.Query().Get("partial") == "1" {
		h.renderFragment(w, http.StatusOK, "task-detail", card)
		return
	}
	h.render(w, http.StatusOK, "detail.html", pageData{
		CSRF:       current.CSRF,
		DetailTask: card,
		Message:    r.URL.Query().Get("message"),
	})
}

func (h *handler) createTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	created, err := h.tasks.Create(webMutationContext(r.Context()), task.CreateInput{
		Title:        r.PostForm.Get("title"),
		Description:  r.PostForm.Get("description"),
		Dependencies: strings.Split(r.PostForm.Get("dependencies"), ","),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	redirectWithMessage(w, r, "Created "+created.ID)
}

// moveTask drops a task into a column at a position. Drag and drop calls it
// with an explicit index and reads the refreshed card back as JSON; the move
// buttons on each card post the same form and follow a redirect.
func (h *handler) moveTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		h.moveFailed(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	index := 0
	if raw := strings.TrimSpace(r.PostForm.Get("index")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			h.moveFailed(w, r, http.StatusBadRequest, "index must be a whole number")
			return
		}
		index = parsed
	}
	status := Status(strings.TrimSpace(r.PostForm.Get("status")))
	moved, err := h.tasks.Move(webMutationContext(r.Context()), r.PathValue("id"), status, index)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, task.ErrBlocked):
			code = http.StatusConflict
		case errors.Is(err, task.ErrNotFound):
			code = http.StatusNotFound
		}
		h.moveFailed(w, r, code, err.Error())
		return
	}
	message := "Moved to " + statusLabel(moved.Status)
	if !wantsJSON(r) {
		redirectWithMessage(w, r, message)
		return
	}
	h.writeCard(w, r, moved, message)
}

// writeCard answers a mutation with the task's freshly rendered board card, so
// the browser never has to rebuild card markup of its own.
func (h *handler) writeCard(w http.ResponseWriter, r *http.Request, item task.Task, message string) {
	current := r.Context().Value(sessionContextKey{}).(session)
	card := h.newTaskCard(item, current.CSRF, h.storedTask(r.Context()))
	var rendered bytes.Buffer
	if err := h.templates.ExecuteTemplate(&rendered, "task-card", card); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "render card"})
		return
	}
	writeJSON(w, http.StatusOK, moveResult{
		ID:          item.ID,
		Status:      item.Status,
		StatusLabel: statusLabel(item.Status),
		Card:        rendered.String(),
		Message:     message,
	})
}

// moveFailed reports a rejected move to whichever client asked for it: JSON for
// drag and drop, plain text for a posted form.
func (h *handler) moveFailed(w http.ResponseWriter, r *http.Request, code int, message string) {
	if wantsJSON(r) {
		writeJSON(w, code, map[string]string{"error": message})
		return
	}
	http.Error(w, message, code)
}

func (h *handler) newTaskCard(item task.Task, csrf string, lookup func(string) (task.Task, bool)) taskCard {
	card := taskCard{
		Task:        item,
		CSRF:        csrf,
		StatusLabel: statusLabel(item.Status),
		Excerpt:     excerpt(item.Description),
		Relative:    h.relativeTime(item.UpdatedAt),
		Timestamp:   item.UpdatedAt.Format(time.RFC3339),
	}
	for _, dependency := range item.Dependencies {
		view := dependencyView{ID: dependency, Title: dependency}
		if found, ok := lookup(dependency); ok {
			view.Title = found.Title
			view.Status = found.Status
			view.StatusLabel = statusLabel(found.Status)
			view.Done = found.Status == task.StatusDone
		}
		if !view.Done {
			card.Blocked = true
		}
		card.DependsOn = append(card.DependsOn, view)
	}
	for index, attachment := range item.Attachments {
		if isImage(attachment.ContentType) {
			card.Cover = &item.Attachments[index]
			break
		}
	}
	for _, status := range []Status{task.StatusTodo, task.StatusInProgress, task.StatusDone} {
		if status != item.Status {
			card.Moves = append(card.Moves, moveOption{Status: status, Label: statusLabel(status)})
		}
	}
	return card
}

// storedTask resolves dependency IDs one at a time, for pages that render a
// single task rather than the whole board.
func (h *handler) storedTask(ctx context.Context) func(string) (task.Task, bool) {
	return func(id string) (task.Task, bool) {
		found, err := h.tasks.Get(ctx, id)
		return found, err == nil
	}
}

func (h *handler) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentSize+(1<<20))
	if err := r.ParseMultipartForm(maxAttachmentSize); err != nil {
		http.Error(w, "invalid file upload", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	file, fileHeader, err := r.FormFile("file")
	if err != nil {
		// Accept the former field name so an upload from a page loaded before
		// this change still succeeds.
		file, fileHeader, err = r.FormFile("image")
		if err != nil {
			http.Error(w, "file is required", http.StatusBadRequest)
			return
		}
	}
	defer file.Close()
	if fileHeader.Size <= 0 || fileHeader.Size > maxAttachmentSize {
		http.Error(w, "file must be between 1 byte and 50 MiB", http.StatusBadRequest)
		return
	}
	leading := make([]byte, 512)
	read, err := io.ReadFull(file, leading)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.Error(w, "read file", http.StatusBadRequest)
		return
	}
	leading = leading[:read]
	contentType := http.DetectContentType(leading)
	name := filepath.Base(fileHeader.Filename)
	if name == "." || name == "" {
		name = "file"
	}
	extension := strings.ToLower(filepath.Ext(name))
	if extension == "" {
		if extensions, _ := mime.ExtensionsByType(contentType); len(extensions) > 0 {
			extension = extensions[0]
		}
	}
	taskID := r.PathValue("id")
	key := taskID + "/" + strings.ToLower(rand.Text()) + extension
	contents := io.MultiReader(bytes.NewReader(leading), file)
	if err := h.objects.Put(r.Context(), key, contents, fileHeader.Size, contentType); err != nil {
		http.Error(w, "store file", http.StatusInternalServerError)
		return
	}
	updated, err := h.tasks.AddAttachment(webMutationContext(r.Context()), taskID, task.Attachment{Key: key, Name: name, ContentType: contentType})
	if err != nil {
		_ = h.objects.Delete(r.Context(), key)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Uploading from the board's detail drawer reads the refreshed card back
	// instead of navigating away from the board.
	if wantsJSON(r) {
		h.writeCard(w, r, updated, "File uploaded")
		return
	}
	http.Redirect(w, r, "/"+taskID+"?message="+url.QueryEscape("File uploaded"), http.StatusSeeOther)
}

func webMutationContext(ctx context.Context) context.Context {
	return task.WithAuditMetadata(ctx, task.AuditMetadata{
		ActorKind: "shared_secret",
		Source:    "web",
	})
}

func (h *handler) attachment(w http.ResponseWriter, r *http.Request) {
	reader, contentType, err := h.objects.Open(r.Context(), r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", contentType)
	disposition := "attachment"
	if isImage(contentType) {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = io.Copy(w, reader)
}

func isImage(contentType string) bool {
	return strings.HasPrefix(contentType, "image/")
}

func (h *handler) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		csrf, expiresAt, ok, err := h.sessions.Session(r.Context(), hashToken(cookie.Value))
		if err != nil {
			http.Error(w, "session lookup failed", http.StatusInternalServerError)
			return
		}
		// A zero expiry never expires. A non-zero expiry belongs to a session
		// issued before infinite sessions and is still honored so it ages out.
		if ok && !expiresAt.IsZero() && !h.now().Before(expiresAt) {
			_ = h.sessions.DeleteSession(r.Context(), hashToken(cookie.Value))
			ok = false
		}
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Slide the browser cookie forward on page loads so an active client is
		// never dropped by the browser's cookie-lifetime cap. Only GETs refresh
		// it, which keeps logout (a POST) free to clear the cookie instead.
		if r.Method == http.MethodGet {
			h.setSessionCookie(w, cookie.Value)
		}
		current := session{CSRF: csrf, ExpiresAt: expiresAt}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sessionContextKey{}, current)))
	})
}

func (h *handler) validCSRF(r *http.Request) bool {
	if err := r.ParseForm(); err != nil {
		return false
	}
	current, ok := r.Context().Value(sessionContextKey{}).(session)
	return ok && current.CSRF != "" && current.CSRF == r.Form.Get("csrf")
}

func (h *handler) render(w http.ResponseWriter, status int, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = h.templates.ExecuteTemplate(w, name, data)
}

func (h *handler) renderFragment(w http.ResponseWriter, status int, name string, data taskCard) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = h.templates.ExecuteTemplate(w, name, data)
}

func redirectWithMessage(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/?message="+url.QueryEscape(message), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func statusLabel(status Status) string {
	switch status {
	case task.StatusTodo:
		return "To do"
	case task.StatusInProgress:
		return "In progress"
	case task.StatusDone:
		return "Done"
	}
	return string(status)
}

// relativeTime renders a timestamp the way a board reader thinks about it,
// falling back to a date once a task has been sitting for a week.
func (h *handler) relativeTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	elapsed := h.now().Sub(at)
	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
	return at.Format("Jan 2")
}

var (
	fencedCode    = regexp.MustCompile("(?s)```.*?```")
	embeddedImage = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	linkedText    = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markupTag     = regexp.MustCompile(`<[^>]*>`)
	// List markers, including the checkbox of a Markdown task list item.
	listMarker    = regexp.MustCompile(`(?m)^\s*([-+*]|\d+\.)\s+(\[[ xX]\]\s*)?`)
	markdownMarks = regexp.MustCompile("[*_`~>#|]+")
	whitespace    = regexp.MustCompile(`\s+`)
)

// excerpt flattens a Markdown description into the single line of plain text a
// board card previews. Cards deliberately show a preview, not the document.
func excerpt(source string) string {
	text := fencedCode.ReplaceAllString(source, " ")
	text = embeddedImage.ReplaceAllString(text, " ")
	text = linkedText.ReplaceAllString(text, "$1")
	text = markupTag.ReplaceAllString(text, " ")
	text = listMarker.ReplaceAllString(text, " ")
	text = markdownMarks.ReplaceAllString(text, "")
	text = strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
	if characters := []rune(text); len(characters) > excerptLimit {
		text = strings.TrimSpace(string(characters[:excerptLimit])) + "…"
	}
	return text
}

func renderMarkdown(source string) template.HTML {
	var output bytes.Buffer
	_ = markdownRenderer.Convert([]byte(source), &output)
	// Goldmark omits raw HTML and dangerous URLs unless explicitly configured
	// as unsafe, so this rendered output is safe to pass through html/template.
	return template.HTML(output.String())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// hashToken returns a hex-encoded SHA-256 of a high-entropy session token, so
// the raw cookie value is never stored.
func hashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// memorySessionStore is a non-durable SessionStore used when no persistent
// store is configured (tests and single-process fallback).
type memorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]memorySession
}

type memorySession struct {
	csrf      string
	expiresAt time.Time
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{sessions: make(map[string]memorySession)}
}

func (m *memorySessionStore) SaveSession(_ context.Context, tokenHash, csrf string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[tokenHash] = memorySession{csrf: csrf, expiresAt: expiresAt}
	return nil
}

func (m *memorySessionStore) Session(_ context.Context, tokenHash string) (string, time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.sessions[tokenHash]
	return current.csrf, current.expiresAt, ok, nil
}

func (m *memorySessionStore) DeleteSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash)
	return nil
}
