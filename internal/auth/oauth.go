package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const taskScope = "tasks"

// Client is a registered OAuth client. Client IDs are public identifiers, so
// they are stored verbatim.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
}

// Code is a stored authorization code. It is keyed by a hash of the code value
// so the raw code never lives at rest.
type Code struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    string
	Scope       string
	ExpiresAt   time.Time
}

// Token is a stored access token, keyed by a hash of the token value.
type Token struct {
	ClientID  string
	Resource  string
	Scope     string
	ExpiresAt time.Time
}

// Store persists OAuth clients, authorization codes, and access tokens. Codes
// and tokens are addressed by a hash of their secret value, never the raw
// value. The production implementation is PostgreSQL; NewServer falls back to
// an in-memory store when none is supplied (used by tests).
type Store interface {
	SaveClient(ctx context.Context, client Client) error
	Client(ctx context.Context, id string) (Client, bool, error)
	SaveCode(ctx context.Context, codeHash string, code Code) error
	// TakeCode atomically returns and deletes the code, enforcing single use.
	TakeCode(ctx context.Context, codeHash string) (Code, bool, error)
	SaveToken(ctx context.Context, tokenHash string, token Token) error
	Token(ctx context.Context, tokenHash string) (Token, bool, error)
}

type Config struct {
	Issuer   string
	Secret   string
	CodeTTL  time.Duration
	TokenTTL time.Duration
	Now      func() time.Time
	// Store persists clients, codes, and tokens. When nil, an in-memory store
	// is used (non-durable; intended for tests and single-process use).
	Store Store
}

type Server struct {
	issuer    string
	resource  string
	secret    [32]byte
	hasSecret bool
	codeTTL   time.Duration
	tokenTTL  time.Duration
	now       func() time.Time
	store     Store
}

func NewServer(config Config) *Server {
	issuer := strings.TrimRight(config.Issuer, "/")
	if config.CodeTTL <= 0 {
		config.CodeTTL = 5 * time.Minute
	}
	if config.TokenTTL <= 0 {
		config.TokenTTL = time.Hour
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Store == nil {
		config.Store = newMemoryStore()
	}
	server := &Server{
		issuer:    issuer,
		resource:  issuer + "/mcp",
		codeTTL:   config.CodeTTL,
		tokenTTL:  config.TokenTTL,
		now:       config.Now,
		store:     config.Store,
		hasSecret: config.Secret != "",
	}
	server.secret = sha256.Sum256([]byte(config.Secret))
	return server
}

func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authorizationServerMetadata)
	mux.HandleFunc("POST /oauth/register", s.register)
	mux.HandleFunc("GET /oauth/authorize", s.authorizePage)
	mux.HandleFunc("POST /oauth/authorize", s.authorize)
	mux.HandleFunc("POST /oauth/token", s.token)
}

func (s *Server) RequireBearer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !s.ValidToken(r.Context(), parts[1]) {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q, scope=%q`, s.issuer+"/.well-known/oauth-protected-resource", taskScope))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ValidToken(ctx context.Context, value string) bool {
	if value == "" {
		return false
	}
	token, ok, err := s.store.Token(ctx, hashSecret(value))
	if err != nil || !ok {
		return false
	}
	if !s.now().Before(token.ExpiresAt) {
		return false
	}
	return token.Resource == s.resource
}

func (s *Server) CheckSecret(value string) bool {
	provided := sha256.Sum256([]byte(value))
	return s.hasSecret && subtle.ConstantTimeCompare(s.secret[:], provided[:]) == 1
}

func (s *Server) protectedResourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.resource,
		"authorization_servers":    []string{s.issuer},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         strings.Fields(taskScope),
	})
}

func (s *Server) authorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"authorization_endpoint":                s.issuer + "/oauth/authorize",
		"token_endpoint":                        s.issuer + "/oauth/token",
		"registration_endpoint":                 s.issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      strings.Fields(taskScope),
	})
}

type registrationRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	var request registrationRequest
	if err := decoder.Decode(&request); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "invalid registration document")
		return
	}
	if len(request.RedirectURIs) == 0 || request.TokenEndpointAuthMethod != "" && request.TokenEndpointAuthMethod != "none" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris and public client authentication are required")
		return
	}
	for _, redirectURI := range request.RedirectURIs {
		if !validRedirectURI(redirectURI) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URI must use HTTPS or an HTTP loopback address")
			return
		}
	}
	// Clients (such as Claude) commonly request extra grant/response types like
	// refresh_token. Per RFC 7591 we register a supported subset rather than
	// rejecting the whole request, as long as the essential ones are present.
	if len(request.GrantTypes) > 0 && !slices.Contains(request.GrantTypes, "authorization_code") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the authorization_code grant is required")
		return
	}
	if len(request.ResponseTypes) > 0 && !slices.Contains(request.ResponseTypes, "code") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "the code response type is required")
		return
	}
	registered := Client{ID: randomText(), Name: request.ClientName, RedirectURIs: slices.Clone(request.RedirectURIs)}
	if err := s.store.SaveClient(r.Context(), registered); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist client registration")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  registered.ID,
		"client_name":                registered.Name,
		"redirect_uris":              registered.RedirectURIs,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

type authorizationRequest struct {
	ClientID    string
	RedirectURI string
	Challenge   string
	Resource    string
	Scope       string
	State       string
}

type authorizationPageData struct {
	authorizationRequest
	Error string
}

func (s *Server) authorizePage(w http.ResponseWriter, r *http.Request) {
	request, err := s.parseAuthorizationRequest(r.Context(), r.URL.Query())
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := authorizationPage.Execute(w, authorizationPageData{authorizationRequest: request}); err != nil {
		http.Error(w, "render authorization page", http.StatusInternalServerError)
	}
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	request, err := s.parseAuthorizationRequest(r.Context(), r.PostForm)
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.CheckSecret(r.PostForm.Get("secret")) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_ = authorizationPage.Execute(w, authorizationPageData{authorizationRequest: request, Error: "That secret code is not valid."})
		return
	}
	code := randomText()
	stored := Code{
		ClientID:    request.ClientID,
		RedirectURI: request.RedirectURI,
		Challenge:   request.Challenge,
		Resource:    request.Resource,
		Scope:       request.Scope,
		ExpiresAt:   s.now().Add(s.codeTTL),
	}
	if err := s.store.SaveCode(r.Context(), hashSecret(code), stored); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist authorization code")
		return
	}
	redirect, _ := url.Parse(request.RedirectURI)
	query := redirect.Query()
	query.Set("code", code)
	if request.State != "" {
		query.Set("state", request.State)
	}
	redirect.RawQuery = query.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *Server) parseAuthorizationRequest(ctx context.Context, values url.Values) (authorizationRequest, error) {
	request := authorizationRequest{
		ClientID:    values.Get("client_id"),
		RedirectURI: values.Get("redirect_uri"),
		Challenge:   values.Get("code_challenge"),
		Resource:    values.Get("resource"),
		Scope:       values.Get("scope"),
		State:       values.Get("state"),
	}
	if values.Get("response_type") != "code" {
		return authorizationRequest{}, errors.New("response_type must be code")
	}
	if values.Get("code_challenge_method") != "S256" || len(request.Challenge) < 43 || len(request.Challenge) > 128 {
		return authorizationRequest{}, errors.New("S256 PKCE is required")
	}
	if request.Resource != s.resource {
		return authorizationRequest{}, errors.New("resource does not identify this MCP server")
	}
	registered, ok, err := s.store.Client(ctx, request.ClientID)
	if err != nil {
		return authorizationRequest{}, errors.New("could not look up client")
	}
	if !ok || !slices.Contains(registered.RedirectURIs, request.RedirectURI) {
		return authorizationRequest{}, errors.New("unknown client or redirect URI")
	}
	if request.Scope == "" {
		request.Scope = taskScope
	}
	for _, requested := range strings.Fields(request.Scope) {
		if !slices.Contains(strings.Fields(taskScope), requested) {
			return authorizationRequest{}, errors.New("unsupported scope")
		}
	}
	return request, nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "invalid form")
		return
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}
	code, ok, err := s.store.TakeCode(r.Context(), hashSecret(r.PostForm.Get("code")))
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not read authorization code")
		return
	}
	if !ok || !s.now().Before(code.ExpiresAt) || code.ClientID != r.PostForm.Get("client_id") || code.RedirectURI != r.PostForm.Get("redirect_uri") || code.Resource != r.PostForm.Get("resource") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	verifier := r.PostForm.Get("code_verifier")
	if len(verifier) < 43 || len(verifier) > 128 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(digest[:])
	if subtle.ConstantTimeCompare([]byte(challenge), []byte(code.Challenge)) != 1 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	value := randomText()
	expiresAt := s.now().Add(s.tokenTTL)
	if err := s.store.SaveToken(r.Context(), hashSecret(value), Token{ClientID: code.ClientID, Resource: code.Resource, Scope: code.Scope, ExpiresAt: expiresAt}); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not persist access token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": value,
		"token_type":   "Bearer",
		"expires_in":   int(s.tokenTTL.Seconds()),
		"scope":        code.Scope,
	})
}

func validRedirectURI(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Fragment != "" || parsed.User != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
}

func randomText() string {
	return rand.Text()
}

// hashSecret returns a hex-encoded SHA-256 of a high-entropy secret (code or
// token). These values are random, so a plain hash is sufficient to keep the
// raw secret out of storage while allowing constant-time lookup by key.
func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func writeOAuthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// memoryStore is a non-durable Store used when no persistent store is
// configured (tests and single-process fallback).
type memoryStore struct {
	mu      sync.Mutex
	clients map[string]Client
	codes   map[string]Code
	tokens  map[string]Token
}

func newMemoryStore() *memoryStore {
	return &memoryStore{
		clients: make(map[string]Client),
		codes:   make(map[string]Code),
		tokens:  make(map[string]Token),
	}
}

func (m *memoryStore) SaveClient(_ context.Context, client Client) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	client.RedirectURIs = slices.Clone(client.RedirectURIs)
	m.clients[client.ID] = client
	return nil
}

func (m *memoryStore) Client(_ context.Context, id string) (Client, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.clients[id]
	if ok {
		client.RedirectURIs = slices.Clone(client.RedirectURIs)
	}
	return client, ok, nil
}

func (m *memoryStore) SaveCode(_ context.Context, codeHash string, code Code) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes[codeHash] = code
	return nil
}

func (m *memoryStore) TakeCode(_ context.Context, codeHash string) (Code, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	code, ok := m.codes[codeHash]
	if ok {
		delete(m.codes, codeHash)
	}
	return code, ok, nil
}

func (m *memoryStore) SaveToken(_ context.Context, tokenHash string, token Token) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[tokenHash] = token
	return nil
}

func (m *memoryStore) Token(_ context.Context, tokenHash string) (Token, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	token, ok := m.tokens[tokenHash]
	return token, ok, nil
}

var authorizationPage = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize Task Tracker</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:10vh auto;padding:1.5rem;color:#17202a}form{display:grid;gap:1rem}input,button{font:inherit;padding:.75rem}button{cursor:pointer;background:#17202a;color:white;border:0;border-radius:.35rem}.error{color:#a00}</style></head>
<body><h1>Authorize Task Tracker</h1><p>Enter the private secret code to let <strong>{{.ClientID}}</strong> read and update tasks.</p>
{{with .Error}}<p class="error">{{.}}</p>{{end}}
<form method="post" action="/oauth/authorize">
<input type="hidden" name="client_id" value="{{.ClientID}}"><input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
<input type="hidden" name="response_type" value="code"><input type="hidden" name="code_challenge" value="{{.Challenge}}">
<input type="hidden" name="code_challenge_method" value="S256"><input type="hidden" name="resource" value="{{.Resource}}">
<input type="hidden" name="scope" value="{{.Scope}}"><input type="hidden" name="state" value="{{.State}}">
<label>Secret code <input type="password" name="secret" required autofocus autocomplete="current-password"></label><button type="submit">Authorize</button>
</form></body></html>`))
