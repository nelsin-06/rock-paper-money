package auth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSessionServicePersistsOpaqueSessionsAcrossRestart(t *testing.T) {
	store := newMemorySessionStore()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	random := bytes.NewReader(append(bytes.Repeat([]byte{0x5a}, sessionTokenBytes), bytes.Repeat([]byte{0x6b}, sessionTokenBytes)...))
	service := NewSessionService(store, random, func() time.Time { return now })

	credentials, err := service.Create(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if credentials.Token == "" || credentials.Token == "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("session token is not opaque: %q", credentials.Token)
	}
	if credentials.CSRFToken == "" || credentials.CSRFToken == credentials.Token {
		t.Fatal("CSRF token must be independent from the session token")
	}

	restarted := NewSessionService(store, bytes.NewReader(nil), func() time.Time { return now.Add(time.Hour) })
	session, err := restarted.Authenticate(context.Background(), credentials.Token)
	if err != nil {
		t.Fatal(err)
	}
	if session.AccountID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("account ID = %q", session.AccountID)
	}
	if session.AbsoluteExpiresAt != now.Add(30*24*time.Hour) {
		t.Fatalf("absolute expiry = %v", session.AbsoluteExpiresAt)
	}
	if session.IdleExpiresAt != now.Add(25*time.Hour) {
		t.Fatalf("renewed idle expiry = %v", session.IdleExpiresAt)
	}
	if !restarted.ValidateCSRF(session, credentials.CSRFToken) {
		t.Fatal("valid session-bound CSRF token was rejected")
	}
}

func TestSessionServiceEnforcesIdleAbsoluteAndRevocationLimits(t *testing.T) {
	tests := []struct {
		name      string
		advance   time.Duration
		revoke    bool
		wantError error
	}{
		{name: "inside both limits", advance: 23 * time.Hour},
		{name: "idle limit", advance: 24 * time.Hour, wantError: ErrInvalidSession},
		{name: "absolute limit", advance: 30 * 24 * time.Hour, wantError: ErrInvalidSession},
		{name: "revoked", advance: time.Minute, revoke: true, wantError: ErrInvalidSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newMemorySessionStore()
			now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
			current := now
			random := bytes.NewReader(append(bytes.Repeat([]byte{byte(len(tt.name) + 1)}, sessionTokenBytes), bytes.Repeat([]byte{byte(len(tt.name) + 2)}, sessionTokenBytes)...))
			service := NewSessionService(store, random, func() time.Time { return current })
			credentials, err := service.Create(context.Background(), "11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			current = current.Add(tt.advance)
			if tt.revoke {
				if err = service.Revoke(context.Background(), credentials.Token); err != nil {
					t.Fatal(err)
				}
			}
			_, err = service.Authenticate(context.Background(), credentials.Token)
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("Authenticate() error = %v, want %v", err, tt.wantError)
			}
		})
	}
}

func TestRequestSecurityRequiresExactOriginAndSessionBoundCSRF(t *testing.T) {
	store := newMemorySessionStore()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	random := append(bytes.Repeat([]byte{0x7b}, sessionTokenBytes), bytes.Repeat([]byte{0x7c}, sessionTokenBytes)...)
	service := NewSessionService(store, bytes.NewReader(random), func() time.Time { return now })
	credentials, err := service.Create(context.Background(), "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.Authenticate(context.Background(), credentials.Token)
	if err != nil {
		t.Fatal(err)
	}
	security, err := NewRequestSecurity("https://game.example", service)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		origin string
		csrf   string
		want   error
	}{
		{name: "valid", origin: "https://game.example", csrf: credentials.CSRFToken},
		{name: "missing origin", csrf: credentials.CSRFToken, want: ErrForbidden},
		{name: "foreign origin", origin: "https://attacker.example", csrf: credentials.CSRFToken, want: ErrForbidden},
		{name: "origin prefix", origin: "https://game.example.attacker.test", csrf: credentials.CSRFToken, want: ErrForbidden},
		{name: "missing CSRF", origin: "https://game.example", want: ErrForbidden},
		{name: "wrong CSRF", origin: "https://game.example", csrf: "wrong", want: ErrForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://game.example/api/rooms", nil)
			if tt.origin != "" {
				request.Header.Set("Origin", tt.origin)
			}
			if tt.csrf != "" {
				request.Header.Set(CSRFHeaderName, tt.csrf)
			}
			if err := security.ValidateMutation(request, session); !errors.Is(err, tt.want) {
				t.Fatalf("ValidateMutation() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSessionCookiesUseSecureBrowserBoundaries(t *testing.T) {
	sessionCookie := NewSessionCookie("opaque")
	if !sessionCookie.HttpOnly || !sessionCookie.Secure || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" {
		t.Fatalf("session cookie = %#v", sessionCookie)
	}
	csrfCookie := NewCSRFCookie("csrf")
	if csrfCookie.HttpOnly || !csrfCookie.Secure || csrfCookie.SameSite != http.SameSiteLaxMode || csrfCookie.Path != "/" {
		t.Fatalf("CSRF cookie = %#v", csrfCookie)
	}
}

func TestSessionHTTPBootstrapAndLogout(t *testing.T) {
	store := newMemorySessionStore()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	random := bytes.NewReader(append(bytes.Repeat([]byte{0x31}, sessionTokenBytes), bytes.Repeat([]byte{0x32}, sessionTokenBytes)...))
	sessions := NewSessionService(store, random, func() time.Time { return now })
	security, err := NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSessionHTTPHandler(sessionVerifierStub{}, sessions, security)

	bootstrap := httptest.NewRequest(http.MethodPost, "/api/session", nil)
	bootstrap.Header.Set("Origin", "https://game.example")
	bootstrap.Header.Set("Authorization", "Bearer verified-account")
	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, bootstrap)
	if bootstrapResponse.Code != http.StatusNoContent {
		t.Fatalf("bootstrap status = %d, want 204; body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	cookies := bootstrapResponse.Result().Cookies()
	if len(cookies) != 2 {
		t.Fatalf("bootstrap cookies = %d, want 2", len(cookies))
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range cookies {
		switch cookie.Name {
		case SessionCookieName:
			sessionCookie = cookie
		case CSRFCookieName:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly || csrfCookie == nil || csrfCookie.HttpOnly {
		t.Fatalf("session=%#v csrf=%#v", sessionCookie, csrfCookie)
	}
	if session, authenticateErr := sessions.Authenticate(context.Background(), sessionCookie.Value); authenticateErr != nil || session.AccountID != "verified-account" {
		t.Fatalf("durable session account=%q error=%v", session.AccountID, authenticateErr)
	}

	for _, test := range []struct {
		name   string
		origin string
		csrf   string
		want   int
	}{
		{name: "missing origin", csrf: csrfCookie.Value, want: http.StatusForbidden},
		{name: "foreign origin", origin: "https://attacker.example", csrf: csrfCookie.Value, want: http.StatusForbidden},
		{name: "missing csrf", origin: "https://game.example", want: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
			request.AddCookie(sessionCookie)
			request.Header.Set("Origin", test.origin)
			request.Header.Set(CSRFHeaderName, test.csrf)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			if _, authenticateErr := sessions.Authenticate(context.Background(), sessionCookie.Value); authenticateErr != nil {
				t.Fatalf("rejected logout revoked session: %v", authenticateErr)
			}
		})
	}

	logout := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	logout.AddCookie(sessionCookie)
	logout.Header.Set("Origin", "https://game.example")
	logout.Header.Set(CSRFHeaderName, csrfCookie.Value)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logoutResponse.Code)
	}
	if _, authenticateErr := sessions.Authenticate(context.Background(), sessionCookie.Value); !errors.Is(authenticateErr, ErrInvalidSession) {
		t.Fatalf("revoked session error = %v", authenticateErr)
	}
	for _, cookie := range logoutResponse.Result().Cookies() {
		if cookie.MaxAge >= 0 {
			t.Fatalf("logout cookie was not cleared: %#v", cookie)
		}
	}
}

func TestRepeatedBootstrapReusesBrowserSessionRevokedByLogout(t *testing.T) {
	store := newMemorySessionStore()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	random := bytes.NewReader(append(bytes.Repeat([]byte{0x41}, sessionTokenBytes), bytes.Repeat([]byte{0x42}, sessionTokenBytes)...))
	sessions := NewSessionService(store, random, func() time.Time { return now })
	security, err := NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSessionHTTPHandler(sessionVerifierStub{}, sessions, security)

	bootstrap := func(cookies ...*http.Cookie) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/session", nil)
		request.Header.Set("Origin", "https://game.example")
		request.Header.Set("Authorization", "Bearer verified-account")
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	first := bootstrap()
	if first.Code != http.StatusNoContent {
		t.Fatalf("first bootstrap status = %d", first.Code)
	}
	var sessionCookie, csrfCookie *http.Cookie
	for _, cookie := range first.Result().Cookies() {
		switch cookie.Name {
		case SessionCookieName:
			sessionCookie = cookie
		case CSRFCookieName:
			csrfCookie = cookie
		}
	}
	if sessionCookie == nil || csrfCookie == nil {
		t.Fatalf("first bootstrap cookies: session=%#v csrf=%#v", sessionCookie, csrfCookie)
	}
	second := bootstrap(sessionCookie, csrfCookie)
	if second.Code != http.StatusNoContent || len(second.Result().Cookies()) != 0 {
		t.Fatalf("repeated bootstrap replaced browser session: status=%d cookies=%#v", second.Code, second.Result().Cookies())
	}

	logout := httptest.NewRequest(http.MethodDelete, "/api/session", nil)
	logout.AddCookie(sessionCookie)
	logout.AddCookie(csrfCookie)
	logout.Header.Set("Origin", "https://game.example")
	logout.Header.Set(CSRFHeaderName, csrfCookie.Value)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", logoutResponse.Code)
	}
	if _, authenticateErr := sessions.Authenticate(context.Background(), sessionCookie.Value); !errors.Is(authenticateErr, ErrInvalidSession) {
		t.Fatalf("socket session authority remained valid after logout: %v", authenticateErr)
	}
}

func TestBootstrapRevokesSessionWhenVerifiedAccountChanges(t *testing.T) {
	store := newMemorySessionStore()
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	randomBytes := make([]byte, 0, 4*sessionTokenBytes)
	for value := byte(0x51); value <= 0x54; value++ {
		randomBytes = append(randomBytes, bytes.Repeat([]byte{value}, sessionTokenBytes)...)
	}
	sessions := NewSessionService(store, bytes.NewReader(randomBytes), func() time.Time { return now })
	security, err := NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewSessionHTTPHandler(sessionVerifierStub{}, sessions, security)

	firstRequest := httptest.NewRequest(http.MethodPost, "/api/session", nil)
	firstRequest.Header.Set("Origin", "https://game.example")
	firstRequest.Header.Set("Authorization", "Bearer account-a")
	firstResponse := httptest.NewRecorder()
	handler.ServeHTTP(firstResponse, firstRequest)
	firstCookies := firstResponse.Result().Cookies()
	if firstResponse.Code != http.StatusNoContent || len(firstCookies) != 2 {
		t.Fatalf("first bootstrap status=%d cookies=%#v", firstResponse.Code, firstCookies)
	}

	secondRequest := httptest.NewRequest(http.MethodPost, "/api/session", nil)
	secondRequest.Header.Set("Origin", "https://game.example")
	secondRequest.Header.Set("Authorization", "Bearer account-b")
	for _, cookie := range firstCookies {
		secondRequest.AddCookie(cookie)
	}
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusNoContent {
		t.Fatalf("replacement bootstrap status=%d", secondResponse.Code)
	}
	var oldToken string
	for _, cookie := range firstCookies {
		if cookie.Name == SessionCookieName {
			oldToken = cookie.Value
		}
	}
	if _, authenticateErr := sessions.Authenticate(context.Background(), oldToken); !errors.Is(authenticateErr, ErrInvalidSession) {
		t.Fatalf("replaced account session remained valid: %v", authenticateErr)
	}
}

type sessionVerifierStub struct{}

func (sessionVerifierStub) Verify(_ context.Context, token string) (Principal, error) {
	if token != "verified-account" && token != "account-a" && token != "account-b" {
		return Principal{}, errors.New("invalid external account")
	}
	return Principal{Subject: token}, nil
}

type memorySessionStore struct {
	mu      sync.Mutex
	records map[Digest]Session
}

func newMemorySessionStore() *memorySessionStore {
	return &memorySessionStore{records: make(map[Digest]Session)}
}

func (s *memorySessionStore) CreateSession(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[session.Digest] = session
	return nil
}

func (s *memorySessionStore) UseSession(_ context.Context, digest Digest, now, idleExpiresAt time.Time) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.records[digest]
	if !ok || session.RevokedAt != nil || !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
		return Session{}, ErrInvalidSession
	}
	if idleExpiresAt.After(session.AbsoluteExpiresAt) {
		idleExpiresAt = session.AbsoluteExpiresAt
	}
	session.LastSeenAt = now
	session.IdleExpiresAt = idleExpiresAt
	s.records[digest] = session
	return session, nil
}

func (s *memorySessionStore) RevokeSession(_ context.Context, digest Digest, revokedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.records[digest]
	if !ok {
		return ErrInvalidSession
	}
	session.RevokedAt = &revokedAt
	s.records[digest] = session
	return nil
}

func (s *memorySessionStore) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var deleted int64
	for digest, session := range s.records {
		if session.RevokedAt != nil || !now.Before(session.IdleExpiresAt) || !now.Before(session.AbsoluteExpiresAt) {
			delete(s.records, digest)
			deleted++
		}
	}
	return deleted, nil
}
