package postgres_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/rock-paper-money/internal/auth"
	roomhttp "example.com/rock-paper-money/internal/room/adapter/http"
	"example.com/rock-paper-money/internal/room/adapter/postgres"
	"example.com/rock-paper-money/internal/room/application"
)

func TestProblemReportPersistence(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	account := "7b82d67d-fad0-48bb-952b-794ac82b599c"
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM problem_reports WHERE account_id=$1", account) })
	sessions := auth.NewSessionService(auth.NewPostgresSessionStore(pool), nil, nil)
	credentials, err := sessions.Create(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	security, err := auth.NewRequestSecurity("https://game.example", sessions)
	if err != nil {
		t.Fatal(err)
	}
	handler := roomhttp.NewSessionRouter(application.NewService(postgres.NewRepository(pool), nil), nil, sessions, security, nil, nil)
	req := httptest.NewRequest("POST", "/api/problem-reports", strings.NewReader(`{"description":"Cannot join","section":"join_room","app_version":"1.0.0"}`))
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: credentials.Token})
	req.Header.Set("Origin", "https://game.example")
	req.Header.Set(auth.CSRFHeaderName, credentials.CSRFToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 201 || w.Body.String() != "{\"status\":\"created\"}\n" {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	var description, section, version string
	var id int64
	var created time.Time
	err = pool.QueryRow(ctx, "SELECT id,description,section,app_version,created_at FROM problem_reports WHERE account_id=$1", account).Scan(&id, &description, &section, &version, &created)
	if err != nil {
		t.Fatal(err)
	}
	if id <= 0 || created.IsZero() || description != "Cannot join" || section != "join_room" || version != "1.0.0" {
		t.Fatalf("unexpected stored report: %d %s %s %s %v", id, description, section, version, created)
	}
}
