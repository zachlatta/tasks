package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"path/filepath"
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
	sessionCookie     = "tasks_session"
	sessionTTL        = 12 * time.Hour
	maxAttachmentSize = 50 << 20
)

//go:embed templates/*.html static/*.css
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
	Error           string
	CSRF            string
	TodoTasks       []taskCard
	InProgressTasks []taskCard
	DoneTasks       []taskCard
	DetailTask      taskCard
	TaskCount       int
	Message         string
	SingleTask      bool
}

type taskCard struct {
	task.Task
	CSRF        string
	Action      string
	ActionLabel string
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
		templates: template.Must(template.New("").Funcs(template.FuncMap{
			"renderMarkdown": renderMarkdown,
			"isImage":        isImage,
		}).ParseFS(assets, "templates/*.html")),
		mux:      http.NewServeMux(),
		sessions: config.Sessions,
	}
	h.mux.HandleFunc("GET /static/app.css", h.styles)
	h.mux.HandleFunc("GET /login", h.loginPage)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.Handle("GET /{$}", h.requireSession(http.HandlerFunc(h.index)))
	h.mux.Handle("POST /logout", h.requireSession(http.HandlerFunc(h.logout)))
	h.mux.Handle("POST /tasks", h.requireSession(http.HandlerFunc(h.createTask)))
	h.mux.Handle("POST /tasks/{id}/start", h.requireSession(http.HandlerFunc(h.startTask)))
	h.mux.Handle("POST /tasks/{id}/done", h.requireSession(http.HandlerFunc(h.completeTask)))
	h.mux.Handle("POST /tasks/{id}/attachments", h.requireSession(http.HandlerFunc(h.uploadAttachment)))
	h.mux.Handle("GET /attachments/{key...}", h.requireSession(http.HandlerFunc(h.attachment)))
	// Keep the image routes working for pages loaded before attachments were
	// generalized and for existing links.
	h.mux.Handle("POST /tasks/{id}/images", h.requireSession(http.HandlerFunc(h.uploadAttachment)))
	h.mux.Handle("GET /images/{key...}", h.requireSession(http.HandlerFunc(h.attachment)))
	h.mux.Handle("GET /{id}", h.requireSession(http.HandlerFunc(h.task)))
	return securityHeaders(h.mux)
}

func (h *handler) styles(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	contents, _ := assets.ReadFile("static/app.css")
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
	now := h.now()
	token := rand.Text()
	current := session{CSRF: rand.Text(), ExpiresAt: now.Add(sessionTTL)}
	if err := h.sessions.SaveSession(r.Context(), hashToken(token), current.CSRF, current.ExpiresAt); err != nil {
		h.render(w, http.StatusInternalServerError, "login.html", pageData{Error: "Could not start a session. Please try again."})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  current.ExpiresAt,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
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

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	items, err := h.reader.Tasks(r.Context())
	if err != nil {
		http.Error(w, "query tasks", http.StatusInternalServerError)
		return
	}
	current := r.Context().Value(sessionContextKey{}).(session)
	data := pageData{
		CSRF:      current.CSRF,
		TaskCount: len(items),
		Message:   r.URL.Query().Get("message"),
	}
	for _, item := range items {
		card := newTaskCard(item, current.CSRF)
		switch item.Status {
		case task.StatusInProgress:
			data.InProgressTasks = append(data.InProgressTasks, card)
		case task.StatusDone:
			data.DoneTasks = append(data.DoneTasks, card)
		default:
			data.TodoTasks = append(data.TodoTasks, card)
		}
	}
	h.render(w, http.StatusOK, "index.html", data)
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
	h.render(w, http.StatusOK, "index.html", pageData{
		CSRF:       current.CSRF,
		DetailTask: newTaskCard(item, current.CSRF),
		Message:    r.URL.Query().Get("message"),
		SingleTask: true,
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
	http.Redirect(w, r, "/?message=Created+"+created.ID, http.StatusSeeOther)
}

func (h *handler) startTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if _, err := h.tasks.Start(webMutationContext(r.Context()), r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/?message=Task+started", http.StatusSeeOther)
}

func (h *handler) completeTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	if _, err := h.tasks.Complete(webMutationContext(r.Context()), r.PathValue("id")); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, task.ErrBlocked) {
			status = http.StatusConflict
		}
		http.Error(w, err.Error(), status)
		return
	}
	http.Redirect(w, r, "/?message=Task+completed", http.StatusSeeOther)
}

func newTaskCard(item task.Task, csrf string) taskCard {
	card := taskCard{Task: item, CSRF: csrf}
	switch item.Status {
	case task.StatusTodo:
		card.Action = "/tasks/" + item.ID + "/start"
		card.ActionLabel = "Start task"
	case task.StatusInProgress:
		card.Action = "/tasks/" + item.ID + "/done"
		card.ActionLabel = "Mark done"
	}
	return card
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
	if _, err := h.tasks.AddAttachment(webMutationContext(r.Context()), taskID, task.Attachment{Key: key, Name: name, ContentType: contentType}); err != nil {
		_ = h.objects.Delete(r.Context(), key)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/?message=File+uploaded", http.StatusSeeOther)
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
		if ok && !h.now().Before(expiresAt) {
			_ = h.sessions.DeleteSession(r.Context(), hashToken(cookie.Value))
			ok = false
		}
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
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

func renderMarkdown(source string) template.HTML {
	var output bytes.Buffer
	_ = markdownRenderer.Convert([]byte(source), &output)
	// Goldmark omits raw HTML and dangerous URLs unless explicitly configured
	// as unsafe, so this rendered output is safe to pass through html/template.
	return template.HTML(output.String())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'self'; form-action 'self'; frame-ancestors 'none'")
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
