package postgres

import (
	"errors"
	"testing"

	"example.com/rock-paper-money/internal/room/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCollectorsReturnIterationErrorsAndCloseRows(t *testing.T) {
	iterationError := errors.New("iteration failed")
	tests := []struct {
		name string
		run  func(*stubRows) error
	}{
		{
			name: "moves",
			run: func(rows *stubRows) error {
				return collectMoves(rows, map[string]domain.Move{})
			},
		},
		{
			name: "next-round requests",
			run: func(rows *stubRows) error {
				return collectNextRoundRequests(rows, map[string]bool{})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := &stubRows{iterationErr: iterationError}
			if err := tt.run(rows); !errors.Is(err, iterationError) {
				t.Fatalf("collector error = %v, want %v", err, iterationError)
			}
			if !rows.closed {
				t.Fatal("rows were not closed")
			}
		})
	}
}

type stubRows struct {
	iterationErr error
	closed       bool
}

func (r *stubRows) Close()                                       { r.closed = true }
func (r *stubRows) Err() error                                   { return r.iterationErr }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Next() bool                                   { r.closed = true; return false }
func (r *stubRows) Scan(...any) error                            { return nil }
func (r *stubRows) Values() ([]any, error)                       { return nil, nil }
func (r *stubRows) RawValues() [][]byte                          { return nil }
func (r *stubRows) Conn() *pgx.Conn                              { return nil }
func (r *stubRows) TypeMap() *pgtype.Map                         { return nil }
