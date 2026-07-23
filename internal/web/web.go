package web

import (
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
	sessionTTL    = 12 * time.Hour
	maxImageSize  = 10 << 20
)

//go:embed templates/*.html static/*.css
var assets embed.FS

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
	Error   string
	CSRF    string
	Tasks   []task.Task
	Message string
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
		templates:     template.Must(template.ParseFS(assets, "templates/*.html")),
		mux:           http.NewServeMux(),
		sessions:      config.Sessions,
	}
	h.mux.HandleFunc("GET /static/app.css", h.styles)
	h.mux.HandleFunc("GET /login", h.loginPage)
	h.mux.HandleFunc("POST /login", h.login)
	h.mux.Handle("GET /", h.requireSession(http.HandlerFunc(h.index)))
	h.mux.Handle("POST /logout", h.requireSession(http.HandlerFunc(h.logout)))
	h.mux.Handle("POST /tasks", h.requireSession(http.HandlerFunc(h.createTask)))
	h.mux.Handle("POST /tasks/{id}/done", h.requireSession(http.HandlerFunc(h.completeTask)))
	h.mux.Handle("POST /tasks/{id}/images", h.requireSession(http.HandlerFunc(h.uploadImage)))
	h.mux.Handle("GET /images/{key...}", h.requireSession(http.HandlerFunc(h.image)))
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
	h.render(w, http.StatusOK, "index.html", pageData{CSRF: current.CSRF, Tasks: items, Message: r.URL.Query().Get("message")})
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

func (h *handler) uploadImage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize+(1<<20))
	if err := r.ParseMultipartForm(maxImageSize); err != nil {
		http.Error(w, "invalid image upload", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !h.validCSRF(r) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	file, fileHeader, err := r.FormFile("image")
	if err != nil {
		http.Error(w, "image is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if fileHeader.Size <= 0 || fileHeader.Size > maxImageSize {
		http.Error(w, "image must be between 1 byte and 10 MiB", http.StatusBadRequest)
		return
	}
	leading := make([]byte, 512)
	read, err := io.ReadFull(file, leading)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		http.Error(w, "read image", http.StatusBadRequest)
		return
	}
	leading = leading[:read]
	contentType := http.DetectContentType(leading)
	if !strings.HasPrefix(contentType, "image/") {
		http.Error(w, "only image uploads are supported", http.StatusUnsupportedMediaType)
		return
	}
	name := filepath.Base(fileHeader.Filename)
	if name == "." || name == "" {
		name = "image"
	}
	extension := strings.ToLower(filepath.Ext(name))
	if extension == "" {
		if extensions, _ := mime.ExtensionsByType(contentType); len(extensions) > 0 {
			extension = extensions[0]
		}
	}
	taskID := r.PathValue("id")
	key := taskID + "/" + strings.ToLower(rand.Text()) + extension
	contents := io.MultiReader(strings.NewReader(string(leading)), file)
	if err := h.objects.Put(r.Context(), key, contents, fileHeader.Size, contentType); err != nil {
		http.Error(w, "store image", http.StatusInternalServerError)
		return
	}
	if _, err := h.tasks.AddAttachment(webMutationContext(r.Context()), taskID, task.Attachment{Key: key, Name: name, ContentType: contentType}); err != nil {
		_ = h.objects.Delete(r.Context(), key)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/?message=Image+uploaded", http.StatusSeeOther)
}

func webMutationContext(ctx context.Context) context.Context {
	return task.WithAuditMetadata(ctx, task.AuditMetadata{
		ActorKind: "shared_secret",
		Source:    "web",
	})
}

func (h *handler) image(w http.ResponseWriter, r *http.Request) {
	reader, contentType, err := h.objects.Open(r.Context(), r.PathValue("key"))
	if err != nil || !strings.HasPrefix(contentType, "image/") {
		http.NotFound(w, r)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, max-age=300")
	_, _ = io.Copy(w, reader)
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
