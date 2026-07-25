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
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
	"github.com/zachlatta/tasks/internal/auth"
	"github.com/zachlatta/tasks/internal/objectstore"
	"github.com/zachlatta/tasks/internal/task"
)

// Reader provides the fixed task projections the pages render: the live board
// and the list of soft-deleted tasks. In production it is the PostgreSQL store;
// tests use an in-memory implementation.
type Reader interface {
	Tasks(ctx context.Context) ([]task.Task, error)
	DeletedTasks(ctx context.Context) ([]task.Task, error)
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

var markdownRenderer = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(
		util.Prioritized(newTabLinkTransformer{}, 100),
	)),
)

type newTabLinkTransformer struct{}

func (newTabLinkTransformer) Transform(document *ast.Document, _ text.Reader, _ parser.Context) {
	_ = ast.Walk(document, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering && (node.Kind() == ast.KindLink || node.Kind() == ast.KindAutoLink) {
			node.SetAttributeString("target", []byte("_blank"))
			node.SetAttributeString("rel", []byte("noopener noreferrer"))
		}
		return ast.WalkContinue, nil
	})
}

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
	Error        string
	CSRF         string
	Columns      []boardColumn
	DetailTask   taskCard
	TaskCount    int
	Deleted      []taskCard
	DeletedCount int
	Message      string
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
	// DeletedRelative and DeletedTimestamp describe when a soft-deleted task
	// left the board, for the list of deleted tasks.
	DeletedRelative  string
	DeletedTimestamp string
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

// deleteResult tells the board which card to take away, and carries the message
// whose undo restores it.
type deleteResult struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// restoreResult carries a restored task's card back to the board along with the
// place in its column it belongs, so an undo puts the card exactly where it was.
type restoreResult struct {
	ID          string `json:"id"`
	Status      Status `json:"status"`
	StatusLabel string `json:"status_label"`
	Card        string `json:"card"`
	Index       int    `json:"index"`
	Message     string `json:"message"`
}

// editResult refreshes both places edited text can be visible: the board card
// and the open task detail.
type editResult struct {
	ID string `json:"id"`
	// Title is the flattened plain-text title used to refresh document.title,
	// never the raw Markdown a title may now carry.
	Title   string `json:"title"`
	Card    string `json:"card"`
	Detail  string `json:"detail"`
	Message string `json:"message"`
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
		"renderMarkdown":       renderMarkdown,
		"renderInlineMarkdown": renderInlineMarkdown,
		"plainTitle":           plainTitle,
		"isImage":              isImage,
	}).ParseFS(assets, "templates/*.html"))
	h.mux.HandleFunc("GET /static/{file}", h.static)
	h.mux.HandleFunc("GET /login", h.loginPage)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.Handle("GET /{$}", h.requireSession(http.HandlerFunc(h.index)))
	h.mux.Handle("POST /logout", h.requireSession(http.HandlerFunc(h.logout)))
	h.mux.Handle("POST /tasks", h.requireSession(http.HandlerFunc(h.createTask)))
	h.mux.Handle("POST /tasks/{id}/edit", h.requireSession(http.HandlerFunc(h.editTask)))
	h.mux.Handle("POST /tasks/{id}/move", h.requireSession(http.HandlerFunc(h.moveTask)))
	h.mux.Handle("POST /tasks/{id}/delete", h.requireSession(http.HandlerFunc(h.deleteTask)))
	h.mux.Handle("POST /tasks/{id}/restore", h.requireSession(http.HandlerFunc(h.restoreTask)))
	h.mux.Handle("GET /deleted", h.requireSession(http.HandlerFunc(h.deletedTasks)))
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
	deleted, err := h.reader.DeletedTasks(r.Context())
	if err != nil {
		http.Error(w, "query deleted tasks", http.StatusInternalServerError)
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
		CSRF:         current.CSRF,
		Columns:      columns,
		TaskCount:    len(items),
		DeletedCount: len(deleted),
		Message:      r.URL.Query().Get("message"),
	})
}

// deletedTasks lists the soft-deleted tasks so they can be read and restored.
// It is the only page that shows a task that has left the board.
func (h *handler) deletedTasks(w http.ResponseWriter, r *http.Request) {
	items, err := h.reader.DeletedTasks(r.Context())
	if err != nil {
		http.Error(w, "query deleted tasks", http.StatusInternalServerError)
		return
	}
	current := r.Context().Value(sessionContextKey{}).(session)
	cards := make([]taskCard, 0, len(items))
	for _, item := range items {
		// The list summarizes deleted tasks; it does not render dependency
		// detail, so nothing needs resolving.
		cards = append(cards, h.newTaskCard(item, current.CSRF, func(string) (task.Task, bool) {
			return task.Task{}, false
		}))
	}
	h.render(w, http.StatusOK, "deleted.html", pageData{
		CSRF:         current.CSRF,
		Deleted:      cards,
		DeletedCount: len(cards),
		Message:      r.URL.Query().Get("message"),
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

// editTask replaces one text field from the inline detail editor. The task
// version rendered with the form prevents a stale open drawer from overwriting
// a newer edit.
func (h *handler) editTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		h.mutationFailed(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	version, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("expected_version")), 10, 64)
	if err != nil || version < 1 {
		h.mutationFailed(w, r, http.StatusBadRequest, "expected version must be a positive whole number")
		return
	}

	value := r.PostForm.Get("value")
	input := task.EditInput{ExpectedVersion: &version}
	message := ""
	switch strings.TrimSpace(r.PostForm.Get("field")) {
	case string(task.TextFieldTitle):
		input.Title = &value
		message = "Updated title"
	case string(task.TextFieldDescription):
		input.Description = &value
		message = "Updated description"
	default:
		h.mutationFailed(w, r, http.StatusBadRequest, "field must be title or description")
		return
	}

	edited, err := h.tasks.Edit(webMutationContext(r.Context()), r.PathValue("id"), input)
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, task.ErrConflict):
			code = http.StatusConflict
		case errors.Is(err, task.ErrNotFound):
			code = http.StatusNotFound
		}
		h.mutationFailed(w, r, code, err.Error())
		return
	}
	if !wantsJSON(r) {
		http.Redirect(w, r, "/"+url.PathEscape(edited.ID)+"?message="+url.QueryEscape(message), http.StatusSeeOther)
		return
	}
	h.writeEdit(w, r, edited, message)
}

func (h *handler) writeEdit(w http.ResponseWriter, r *http.Request, item task.Task, message string) {
	current := r.Context().Value(sessionContextKey{}).(session)
	card := h.newTaskCard(item, current.CSRF, h.storedTask(r.Context()))
	var cardHTML bytes.Buffer
	if err := h.templates.ExecuteTemplate(&cardHTML, "task-card", card); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "render card"})
		return
	}
	var detailHTML bytes.Buffer
	if err := h.templates.ExecuteTemplate(&detailHTML, "task-detail", card); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "render task detail"})
		return
	}
	writeJSON(w, http.StatusOK, editResult{
		ID:      item.ID,
		Title:   plainTitle(item.Title),
		Card:    cardHTML.String(),
		Detail:  detailHTML.String(),
		Message: message,
	})
}

// moveTask drops a task into a column at a position. Drag and drop calls it
// with an explicit index and reads the refreshed card back as JSON; the move
// buttons on each card post the same form and follow a redirect.
func (h *handler) moveTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		h.mutationFailed(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	index := 0
	if raw := strings.TrimSpace(r.PostForm.Get("index")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			h.mutationFailed(w, r, http.StatusBadRequest, "index must be a whole number")
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
		h.mutationFailed(w, r, code, err.Error())
		return
	}
	message := "Moved to " + statusLabel(moved.Status)
	if !wantsJSON(r) {
		redirectWithMessage(w, r, message)
		return
	}
	h.writeCard(w, r, moved, message)
}

// deleteTask soft-deletes a task. The board drops the card and offers an undo
// that restores it; a posted form redirects back to the board instead.
func (h *handler) deleteTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		h.mutationFailed(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	deleted, err := h.tasks.Delete(webMutationContext(r.Context()), r.PathValue("id"))
	if err != nil {
		h.mutationFailed(w, r, statusForTaskError(err), err.Error())
		return
	}
	message := "Deleted " + deleted.ID
	if !wantsJSON(r) {
		redirectWithMessage(w, r, message)
		return
	}
	writeJSON(w, http.StatusOK, deleteResult{ID: deleted.ID, Message: message})
}

// restoreTask puts a soft-deleted task back on the board, either as the board's
// undo or from the list of deleted tasks.
func (h *handler) restoreTask(w http.ResponseWriter, r *http.Request) {
	if !h.validCSRF(r) {
		h.mutationFailed(w, r, http.StatusForbidden, "invalid CSRF token")
		return
	}
	restored, err := h.tasks.Restore(webMutationContext(r.Context()), r.PathValue("id"))
	if err != nil {
		h.mutationFailed(w, r, statusForTaskError(err), err.Error())
		return
	}
	message := "Restored " + restored.ID
	if !wantsJSON(r) {
		http.Redirect(w, r, "/deleted?message="+url.QueryEscape(message), http.StatusSeeOther)
		return
	}
	current := r.Context().Value(sessionContextKey{}).(session)
	card := h.newTaskCard(restored, current.CSRF, h.storedTask(r.Context()))
	var rendered bytes.Buffer
	if err := h.templates.ExecuteTemplate(&rendered, "task-card", card); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "render card"})
		return
	}
	writeJSON(w, http.StatusOK, restoreResult{
		ID:          restored.ID,
		Status:      restored.Status,
		StatusLabel: statusLabel(restored.Status),
		Card:        rendered.String(),
		Index:       h.columnIndex(r.Context(), restored),
		Message:     message,
	})
}

// columnIndex reports where a task sits in its own column, top card first, so
// the board can reinsert a restored card exactly where it belongs.
func (h *handler) columnIndex(ctx context.Context, item task.Task) int {
	items, err := h.reader.Tasks(ctx)
	if err != nil {
		return 0
	}
	index := 0
	for _, other := range items {
		if other.Status != item.Status {
			continue
		}
		if other.ID == item.ID {
			return index
		}
		index++
	}
	return 0
}

// statusForTaskError maps a refused delete or restore onto the closest HTTP
// status, so both the board and a posted form report the real reason.
func statusForTaskError(err error) int {
	switch {
	case errors.Is(err, task.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, task.ErrHasDependents),
		errors.Is(err, task.ErrDependencyNotFound),
		errors.Is(err, task.ErrConflict):
		return http.StatusConflict
	}
	return http.StatusBadRequest
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

// mutationFailed reports a rejected task change to whichever client asked for
// it: JSON for the board's own requests, plain text for a posted form.
func (h *handler) mutationFailed(w http.ResponseWriter, r *http.Request, code int, message string) {
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
	if item.Deleted() {
		card.DeletedRelative = h.relativeTime(*item.DeletedAt)
		card.DeletedTimestamp = item.DeletedAt.Format(time.RFC3339)
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
	// markdownEscape matches a backslash escaping ASCII punctuation, which the
	// rich editor emits so literal markers survive a round trip. Flattening
	// drops the backslash so previews and titles read as plain text.
	markdownEscape = regexp.MustCompile("\\\\([!-/:-@\\[-\x60{-~])")
)

// flattenMarkdown reduces Markdown to a single line of plain text, dropping
// structure and markup while keeping the words a reader would speak.
func flattenMarkdown(source string) string {
	text := markdownEscape.ReplaceAllString(source, "$1")
	text = fencedCode.ReplaceAllString(text, " ")
	text = embeddedImage.ReplaceAllString(text, " ")
	text = linkedText.ReplaceAllString(text, "$1")
	text = markupTag.ReplaceAllString(text, " ")
	text = listMarker.ReplaceAllString(text, " ")
	text = markdownMarks.ReplaceAllString(text, "")
	return strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
}

// excerpt flattens a Markdown description into the single line of plain text a
// board card previews. Cards deliberately show a preview, not the document.
func excerpt(source string) string {
	text := flattenMarkdown(source)
	if characters := []rune(text); len(characters) > excerptLimit {
		text = strings.TrimSpace(string(characters[:excerptLimit])) + "…"
	}
	return text
}

// plainTitle flattens a Markdown title to plain text for the places a title
// must not carry markup: the browser tab title, a card's accessible name, and
// the JSON a live edit uses to refresh document.title.
func plainTitle(source string) string {
	return flattenMarkdown(source)
}

func renderMarkdown(source string) template.HTML {
	var output bytes.Buffer
	_ = markdownRenderer.Convert([]byte(source), &output)
	// Goldmark omits raw HTML and dangerous URLs unless explicitly configured
	// as unsafe, so this rendered output is safe to pass through html/template.
	return template.HTML(output.String())
}

var (
	// inlineParagraph matches the lone paragraph goldmark wraps around a
	// single line of Markdown, so a title can render as inline HTML.
	inlineParagraph = regexp.MustCompile(`(?s)\A<p>(.*)</p>\z`)
	// anchorTag matches a link's opening or closing tag. Titles render inside a
	// card's own anchor, which must never nest another link.
	anchorTag = regexp.MustCompile(`</?a\b[^>]*>`)
)

// renderInlineMarkdown renders a task title's Markdown as inline HTML. It keeps
// emphasis, strong, strikethrough, and code, but strips the wrapping paragraph
// and any links so the result is safe to drop inside a card's own anchor.
func renderInlineMarkdown(source string) template.HTML {
	var output bytes.Buffer
	_ = markdownRenderer.Convert([]byte(source), &output)
	rendered := strings.TrimRight(output.String(), "\n")
	if match := inlineParagraph.FindStringSubmatch(rendered); match != nil {
		rendered = match[1]
	}
	rendered = anchorTag.ReplaceAllString(rendered, "")
	return template.HTML(rendered)
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
