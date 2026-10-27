package roomhttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/room/application"
)

type reportRepository struct {
	application.Repository
	reports []application.ProblemReport
	fail    bool
}

func (s *reportRepository) SaveProblemReport(_ context.Context, report application.ProblemReport) error {
	if s.fail {
		return errors.New("database unavailable")
	}
	s.reports = append(s.reports, report)
	return nil
}

func TestProblemReports(t *testing.T) {
	sessions := auth.NewSessionService(newRouterSessionStore(), nil, nil)
	credentials, err := sessions.Create(context.Background(), "7b82d67d-fad0-48bb-952b-794ac82b599c")
	if err != nil {
		t.Fatal(err)
	}
	security, err := auth.NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	store := &reportRepository{}
	handler := NewSessionRouter(application.NewService(store, nil), testVerifier{}, sessions, security, nil, nil)
	valid := `{"description":"Cannot join","section":"join_room","app_version":"1.0.0"}`
	for _, tt := range []struct {
		name, body, cookie, csrf, origin string
		status                           int
	}{
		{"valid", valid, credentials.Token, credentials.CSRFToken, "https://game.example", 201},
		{"anonymous", valid, "", "", "https://game.example", 401},
		{"invalid cookie", valid, "invalid", credentials.CSRFToken, "https://game.example", 401},
		{"csrf", valid, credentials.Token, "", "https://game.example", 403},
		{"origin", valid, credentials.Token, credentials.CSRFToken, "https://evil.example", 403},
		{"section", strings.Replace(valid, "join_room", "unknown", 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"empty", strings.Replace(valid, "Cannot join", "   ", 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"long description", strings.Replace(valid, "Cannot join", strings.Repeat("a", 5001), 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"empty version", strings.Replace(valid, "1.0.0", "", 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"long version", strings.Replace(valid, "1.0.0", strings.Repeat("v", 65), 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"spoof account", strings.Replace(valid, "{", `{"account_id":"other",`, 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"email", strings.Replace(valid, "{", `{"email":"x@example.com",`, 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"full report", strings.Replace(valid, "{", `{"report_text":"duplicate",`, 1), credentials.Token, credentials.CSRFToken, "https://game.example", 400},
		{"trailing body", valid + ` {}`, credentials.Token, credentials.CSRFToken, "https://game.example", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(store.reports)
			req := httptest.NewRequest("POST", "/api/problem-reports", strings.NewReader(tt.body))
			req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tt.cookie})
			req.Header.Set("Origin", tt.origin)
			req.Header.Set(auth.CSRFHeaderName, tt.csrf)
			req.Header.Set("Authorization", "Bearer host-user")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			assertStatus(t, w, tt.status)
			if tt.status != 201 {
				if len(store.reports) != before {
					t.Fatal("rejected request saved a report")
				}
			} else {
				if w.Body.String() != "{\"status\":\"created\"}\n" || len(store.reports) != before+1 {
					t.Fatalf("response=%s reports=%v", w.Body.String(), store.reports)
				}
				report := store.reports[before]
				if report.AccountID != "7b82d67d-fad0-48bb-952b-794ac82b599c" || report.Description != "Cannot join" || report.Section != "join_room" || report.AppVersion != "1.0.0" {
					t.Fatalf("report=%+v", report)
				}
			}
		})
	}
	store.fail = true
	req := httptest.NewRequest("POST", "/api/problem-reports", strings.NewReader(valid))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	req.Header.Set("Origin", "https://game.example")
	req.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	assertStatus(t, w, 500)
	if len(store.reports) != 1 || strings.Contains(w.Body.String(), "database unavailable") {
		t.Fatal("failed insert changed state or leaked error")
	}
}
