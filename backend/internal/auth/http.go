package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	SessionCookieName = "rpm_session"
	CSRFCookieName    = "rpm_csrf"
	CSRFHeaderName    = "X-CSRF-Token"
)

var ErrForbidden = errors.New("request forbidden")

type RequestSecurity struct {
	allowedOrigin string
	sessions      *SessionService
}

func NewRequestSecurity(allowedOrigin string, sessions *SessionService) (*RequestSecurity, error) {
	origin, err := url.Parse(allowedOrigin)
	if err != nil || (origin.Scheme != "https" && origin.Scheme != "http") || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return nil, errors.New("allowed origin must be an HTTP origin")
	}
	return &RequestSecurity{allowedOrigin: origin.String(), sessions: sessions}, nil
}

func (s *RequestSecurity) ValidateOrigin(request *http.Request) error {
	values := request.Header.Values("Origin")
	if len(values) != 1 || values[0] != s.allowedOrigin {
		return ErrForbidden
	}
	return nil
}

func (s *RequestSecurity) ValidateMutation(request *http.Request, session Session) error {
	if err := s.ValidateOrigin(request); err != nil {
		return err
	}
	values := request.Header.Values(CSRFHeaderName)
	if len(values) != 1 || !s.sessions.ValidateCSRF(session, values[0]) {
		return ErrForbidden
	}
	return nil
}

func NewSessionCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookieName, Value: token, Path: "/", MaxAge: int(SessionAbsoluteLimit.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}
}

func NewCSRFCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name: CSRFCookieName, Value: token, Path: "/", MaxAge: int(SessionAbsoluteLimit.Seconds()),
		Secure: true, HttpOnly: false, SameSite: http.SameSiteLaxMode,
	}
}

type sessionHTTPHandler struct {
	verifier Verifier
	sessions *SessionService
	security *RequestSecurity
}

// NewSessionHTTPHandler exchanges one verified external account token for an
// opaque browser session and owns durable logout for that session.
func NewSessionHTTPHandler(verifier Verifier, sessions *SessionService, security *RequestSecurity) http.Handler {
	return &sessionHTTPHandler{verifier: verifier, sessions: sessions, security: security}
}

func (h *sessionHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/session" {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPost:
		h.bootstrap(w, r)
	case http.MethodDelete:
		h.logout(w, r)
	default:
		w.Header().Set("Allow", http.MethodPost+", "+http.MethodDelete)
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

func (h *sessionHTTPHandler) bootstrap(w http.ResponseWriter, r *http.Request) {
	if h.verifier == nil || h.sessions == nil || h.security == nil || h.security.ValidateOrigin(r) != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	token, ok := bearerToken(r)
	if !ok {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	principal, err := h.verifier.Verify(r.Context(), token)
	if err != nil || principal.Subject == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if cookie, cookieErr := r.Cookie(SessionCookieName); cookieErr == nil && cookie.Value != "" {
		current, authenticateErr := h.sessions.Authenticate(r.Context(), cookie.Value)
		if authenticateErr == nil {
			csrfCookie, csrfErr := r.Cookie(CSRFCookieName)
			if current.AccountID == principal.Subject && csrfErr == nil && h.sessions.ValidateCSRF(current, csrfCookie.Value) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if revokeErr := h.sessions.Revoke(r.Context(), cookie.Value); revokeErr != nil {
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
		} else if !errors.Is(authenticateErr, ErrInvalidSession) {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}
	credentials, err := h.sessions.Create(r.Context(), principal.Subject)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, NewSessionCookie(credentials.Token))
	http.SetCookie(w, NewCSRFCookie(credentials.CSRFToken))
	w.WriteHeader(http.StatusNoContent)
}

func (h *sessionHTTPHandler) logout(w http.ResponseWriter, r *http.Request) {
	if h.sessions == nil || h.security == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	session, err := h.sessions.Authenticate(r.Context(), cookie.Value)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if h.security.ValidateMutation(r, session) != nil {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if err = h.sessions.Revoke(r.Context(), cookie.Value); err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, expiredCookie(SessionCookieName, true))
	http.SetCookie(w, expiredCookie(CSRFCookieName, false))
	w.WriteHeader(http.StatusNoContent)
}

func expiredCookie(name string, httpOnly bool) *http.Cookie {
	return &http.Cookie{Name: name, Path: "/", MaxAge: -1, Expires: time.Unix(1, 0), Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode}
}

func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

var _ interface {
	Authenticate(context.Context, string) (Session, error)
} = (*SessionService)(nil)
