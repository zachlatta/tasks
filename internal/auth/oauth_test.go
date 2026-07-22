package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestOAuthAuthorizationCodeFlow(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{
		Issuer: "https://tasks.example.com",
		Secret: "correct horse battery staple",
	})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	clientID := registerClient(t, mux, "http://127.0.0.1/callback")

	verifier := "0123456789012345678901234567890123456789012"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	form := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://tasks.example.com/mcp"},
		"state":                 {"state-123"},
		"secret":                {"correct horse battery staple"},
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+form.Encode(), nil)
	pageResponse := httptest.NewRecorder()
	mux.ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK || !strings.Contains(pageResponse.Body.String(), `name="secret"`) {
		t.Fatalf("authorize page status = %d; body: %s", pageResponse.Code, pageResponse.Body.String())
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)

	if response.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, want %d; body: %s", response.Code, http.StatusFound, response.Body.String())
	}
	redirect, err := url.Parse(response.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := redirect.Query().Get("code")
	if code == "" || redirect.Query().Get("state") != "state-123" {
		t.Fatalf("redirect query = %q, want code and original state", redirect.RawQuery)
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"redirect_uri":  {"http://127.0.0.1/callback"},
		"resource":      {"https://tasks.example.com/mcp"},
		"code":          {code},
		"code_verifier": {verifier},
	}
	tokenRequest := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResponse := httptest.NewRecorder()
	mux.ServeHTTP(tokenResponse, tokenRequest)

	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token status = %d, want %d; body: %s", tokenResponse.Code, http.StatusOK, tokenResponse.Body.String())
	}
	var tokenBody struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.Unmarshal(tokenResponse.Body.Bytes(), &tokenBody); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if tokenBody.AccessToken == "" || tokenBody.TokenType != "Bearer" {
		t.Fatalf("token response = %#v", tokenBody)
	}
	if !server.ValidToken(context.Background(), tokenBody.AccessToken) {
		t.Fatal("issued token is not valid")
	}

	// The authorization code is single use: a replayed exchange must fail.
	replay := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	replay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayResponse := httptest.NewRecorder()
	mux.ServeHTTP(replayResponse, replay)
	if replayResponse.Code != http.StatusBadRequest {
		t.Fatalf("code replay status = %d, want %d", replayResponse.Code, http.StatusBadRequest)
	}
}

func TestRegisterAcceptsRefreshTokenGrant(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	// Claude's connector registers requesting authorization_code AND
	// refresh_token; the server must register the supported subset, not reject.
	body := `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],` +
		`"grant_types":["authorization_code","refresh_token"],"response_types":["code"],` +
		`"token_endpoint_auth_method":"none"}`
	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d; body: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var registration struct {
		ClientID   string   `json:"client_id"`
		GrantTypes []string `json:"grant_types"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.ClientID == "" {
		t.Fatal("registration returned an empty client_id")
	}
	if len(registration.GrantTypes) != 1 || registration.GrantTypes[0] != "authorization_code" {
		t.Fatalf("registered grant_types = %v, want [authorization_code]", registration.GrantTypes)
	}
}

func TestRegisterRejectsMissingAuthorizationCodeGrant(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)

	body := `{"client_name":"x","redirect_uris":["https://claude.ai/api/mcp/auth_callback"],` +
		`"grant_types":["client_credentials"],"token_endpoint_auth_method":"none"}`
	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("register status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestOAuthRejectsWrongSecret(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "right"})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	clientID := registerClient(t, mux, "http://127.0.0.1/callback")
	form := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1/callback"},
		"response_type":         {"code"},
		"code_challenge":        {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		"code_challenge_method": {"S256"},
		"resource":              {"https://tasks.example.com/mcp"},
		"secret":                {"wrong"},
	}
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func registerClient(t *testing.T, handler http.Handler, redirectURI string) string {
	t.Helper()
	body := `{"client_name":"test client","client_uri":"https://client.example.com","redirect_uris":["` + redirectURI + `"],"token_endpoint_auth_method":"none"}`
	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("register status = %d, want %d; body: %s", response.Code, http.StatusCreated, response.Body.String())
	}
	var registration struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &registration); err != nil {
		t.Fatalf("decode registration: %v", err)
	}
	if registration.ClientID == "" {
		t.Fatal("registration returned an empty client_id")
	}
	return registration.ClientID
}

func TestBearerMiddlewareAdvertisesProtectedResource(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	handler := server.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	want := `resource_metadata="https://tasks.example.com/.well-known/oauth-protected-resource"`
	if !strings.Contains(response.Header().Get("WWW-Authenticate"), want) {
		t.Fatalf("WWW-Authenticate = %q, want it to contain %q", response.Header().Get("WWW-Authenticate"), want)
	}
}
