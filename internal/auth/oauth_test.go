package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
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
	if body := pageResponse.Body.String(); !strings.Contains(body, "Authorize Tasks") {
		t.Fatalf("authorize page identity is stale; body: %s", body)
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

func TestAuthorizeAcceptsAnyResourceIndicator(t *testing.T) {
	t.Parallel()

	// Clients vary in how they send the RFC 8707 resource indicator: some omit
	// it entirely (as Claude's connector does), others send a slightly different
	// form. The server hosts a single MCP resource, so every one of these must
	// connect and receive a token bound to this server.
	for _, resource := range []string{"", "https://tasks.example.com", "https://tasks.example.com/mcp"} {
		resource := resource
		name := resource
		if name == "" {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := NewServer(Config{
				Issuer: "https://tasks.example.com",
				Secret: "correct horse battery staple",
			})
			mux := http.NewServeMux()
			server.RegisterRoutes(mux)
			clientID := registerClient(t, mux, "https://claude.ai/api/mcp/auth_callback")

			verifier := "0123456789012345678901234567890123456789012"
			sum := sha256.Sum256([]byte(verifier))
			challenge := base64.RawURLEncoding.EncodeToString(sum[:])
			form := url.Values{
				"client_id":             {clientID},
				"redirect_uri":          {"https://claude.ai/api/mcp/auth_callback"},
				"response_type":         {"code"},
				"code_challenge":        {challenge},
				"code_challenge_method": {"S256"},
				"state":                 {"state-123"},
				"secret":                {"correct horse battery staple"},
			}
			if resource != "" {
				form.Set("resource", resource)
			}

			pageRequest := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+form.Encode(), nil)
			pageResponse := httptest.NewRecorder()
			mux.ServeHTTP(pageResponse, pageRequest)
			if pageResponse.Code != http.StatusOK {
				t.Fatalf("authorize page status = %d, want %d; body: %s", pageResponse.Code, http.StatusOK, pageResponse.Body.String())
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
			if code == "" {
				t.Fatalf("redirect query = %q, want a code", redirect.RawQuery)
			}

			tokenForm := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {clientID},
				"redirect_uri":  {"https://claude.ai/api/mcp/auth_callback"},
				"code":          {code},
				"code_verifier": {verifier},
			}
			if resource != "" {
				tokenForm.Set("resource", resource)
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
			}
			if err := json.Unmarshal(tokenResponse.Body.Bytes(), &tokenBody); err != nil {
				t.Fatalf("decode token response: %v", err)
			}
			if !server.ValidToken(context.Background(), tokenBody.AccessToken) {
				t.Fatal("issued token is not valid")
			}
		})
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
	if !slices.Contains(registration.GrantTypes, "authorization_code") || !slices.Contains(registration.GrantTypes, "refresh_token") {
		t.Fatalf("registered grant_types = %v, want authorization_code and refresh_token", registration.GrantTypes)
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

// exchangeAuthorizationCode runs authorize + token and returns the decoded
// token response.
func exchangeAuthorizationCode(t *testing.T, handler http.Handler, clientID, secret string) map[string]any {
	t.Helper()
	verifier := "0123456789012345678901234567890123456789012"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authForm := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"http://127.0.0.1/callback"},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {"https://tasks.example.com/mcp"},
		"secret":                {secret},
	}
	authRequest := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(authForm.Encode()))
	authRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authResponse := httptest.NewRecorder()
	handler.ServeHTTP(authResponse, authRequest)
	if authResponse.Code != http.StatusFound {
		t.Fatalf("authorize status = %d; body: %s", authResponse.Code, authResponse.Body.String())
	}
	redirect, err := url.Parse(authResponse.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := redirect.Query().Get("code")
	if code == "" {
		t.Fatal("authorize returned no code")
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
	handler.ServeHTTP(tokenResponse, tokenRequest)
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token status = %d; body: %s", tokenResponse.Code, tokenResponse.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(tokenResponse.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return body
}

func postForm(t *testing.T, handler http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestRefreshTokenGrantIssuesNewAccessToken(t *testing.T) {
	t.Parallel()

	const secret = "refresh-secret"
	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: secret})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	clientID := registerClient(t, mux, "http://127.0.0.1/callback")

	first := exchangeAuthorizationCode(t, mux, clientID, secret)
	refresh, _ := first["refresh_token"].(string)
	if refresh == "" {
		t.Fatalf("token response has no refresh_token: %#v", first)
	}
	access, _ := first["access_token"].(string)
	if !server.ValidToken(context.Background(), access) {
		t.Fatal("initial access token is not valid")
	}

	response := postForm(t, mux, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("refresh status = %d; body: %s", response.Code, response.Body.String())
	}
	var refreshed map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	newAccess, _ := refreshed["access_token"].(string)
	if newAccess == "" || newAccess == access {
		t.Fatalf("refresh did not issue a new access token: %#v", refreshed)
	}
	if !server.ValidToken(context.Background(), newAccess) {
		t.Fatal("refreshed access token is not valid")
	}
	// The refresh token must keep working so the user only authorizes once.
	if got, _ := refreshed["refresh_token"].(string); got != refresh {
		t.Fatalf("refresh_token = %q, want the original to remain valid", got)
	}
	again := postForm(t, mux, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {clientID},
	})
	if again.Code != http.StatusOK {
		t.Fatalf("second refresh status = %d, want reusable refresh token; body: %s", again.Code, again.Body.String())
	}
}

func TestRefreshTokenRejectsUnknownAndMismatchedClient(t *testing.T) {
	t.Parallel()

	const secret = "refresh-secret"
	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: secret})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	clientID := registerClient(t, mux, "http://127.0.0.1/callback")
	issued := exchangeAuthorizationCode(t, mux, clientID, secret)
	refresh, _ := issued["refresh_token"].(string)

	unknown := postForm(t, mux, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"not-a-real-refresh-token"},
		"client_id":     {clientID},
	})
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown refresh token status = %d, want %d", unknown.Code, http.StatusBadRequest)
	}

	mismatched := postForm(t, mux, "/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {"some-other-client"},
	})
	if mismatched.Code != http.StatusBadRequest {
		t.Fatalf("mismatched client status = %d, want %d", mismatched.Code, http.StatusBadRequest)
	}
}

func TestRefreshTokenIgnoresResourceIndicatorVariations(t *testing.T) {
	t.Parallel()

	const secret = "refresh-secret"
	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: secret})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	clientID := registerClient(t, mux, "http://127.0.0.1/callback")
	issued := exchangeAuthorizationCode(t, mux, clientID, secret)
	refresh, _ := issued["refresh_token"].(string)
	if refresh == "" {
		t.Fatalf("no refresh_token issued: %#v", issued)
	}

	// Clients send the RFC 8707 resource indicator inconsistently. None of these
	// forms may break a refresh, or the user would have to authorize again.
	for _, resource := range []string{"", "https://tasks.example.com", "https://tasks.example.com/mcp"} {
		form := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
			"client_id":     {clientID},
		}
		if resource != "" {
			form.Set("resource", resource)
		}
		response := postForm(t, mux, "/oauth/token", form)
		if response.Code != http.StatusOK {
			t.Fatalf("refresh with resource %q status = %d; body: %s", resource, response.Code, response.Body.String())
		}
	}
}

func TestMetadataAdvertisesRefreshTokenGrant(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	mux := http.NewServeMux()
	server.RegisterRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil))

	var metadata struct {
		GrantTypesSupported []string `json:"grant_types_supported"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if !slices.Contains(metadata.GrantTypesSupported, "refresh_token") {
		t.Fatalf("grant_types_supported = %v, want it to include refresh_token", metadata.GrantTypesSupported)
	}
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

func TestBearerMiddlewareAddsOAuthClientToContext(t *testing.T) {
	t.Parallel()

	server := NewServer(Config{Issuer: "https://tasks.example.com", Secret: "secret"})
	const accessToken = "valid-access-token"
	if err := server.store.SaveToken(context.Background(), hashSecret(accessToken), Token{
		ClientID:  "client-123",
		Resource:  "https://tasks.example.com/mcp",
		Scope:     "tasks",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}
	handler := server.RequireBearer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := ClientIDFromContext(r.Context())
		if !ok || clientID != "client-123" {
			t.Errorf("client identity = %q, %v; want client-123, true", clientID, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body: %s", response.Code, http.StatusNoContent, response.Body.String())
	}
}
