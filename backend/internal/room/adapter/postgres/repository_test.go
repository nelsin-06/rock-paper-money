package postgres

import (
	"reflect"
	"strings"
	"testing"

	"example.com/rock-paper-money/internal/room/application"
	"example.com/rock-paper-money/internal/room/domain"
)

func TestDecodeAggregateRows(t *testing.T) {
	state := domain.PersistenceState{Moves: map[string]domain.Move{}, NextRoundRequests: map[string]bool{}}
	roles := map[string]string{}
	err := decodeAggregateRows(
		[]byte(`[{"role":"host","player_id":"host-id","wins":2},{"role":"guest","player_id":"guest-id","wins":1}]`),
		[]byte(`[{"player_id":"guest-id","move":"paper"}]`),
		[]byte(`[{"player_id":"host-id"}]`),
		&state,
		roles,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantPlayers := []domain.PersistedPlayer{{ID: "host-id", Wins: 2}, {ID: "guest-id", Wins: 1}}
	if !reflect.DeepEqual(state.Players, wantPlayers) || roles["host"] != "host-id" || roles["guest"] != "guest-id" {
		t.Fatalf("decoded seats: players=%+v roles=%+v", state.Players, roles)
	}
	if state.Moves["guest-id"] != domain.Paper || !state.NextRoundRequests["host-id"] {
		t.Fatalf("decoded round state: moves=%+v requests=%+v", state.Moves, state.NextRoundRequests)
	}
}

func TestDecodeAggregateRowsRejectsMalformedJSON(t *testing.T) {
	tests := []struct {
		name     string
		seats    string
		moves    string
		requests string
		want     string
	}{
		{name: "seats", seats: `{`, moves: `[]`, requests: `[]`, want: "decode room seats"},
		{name: "moves", seats: `[]`, moves: `{`, requests: `[]`, want: "decode room moves"},
		{name: "requests", seats: `[]`, moves: `[]`, requests: `{`, want: "decode next-round requests"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := domain.PersistenceState{Moves: map[string]domain.Move{}, NextRoundRequests: map[string]bool{}}
			err := decodeAggregateRows([]byte(tt.seats), []byte(tt.moves), []byte(tt.requests), &state, map[string]string{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("decodeAggregateRows() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSettlementEffectsRemainBalanced(t *testing.T) {
	tests := []struct {
		name        string
		result      domain.Result
		wantIDs     []string
		wantAmounts []int64
		wantRole    string
		wantHouse   int64
	}{
		{name: "draw refunds both players", result: domain.Draw, wantIDs: []string{"escrow", "host", "guest"}, wantAmounts: []int64{-100, 50, 50}},
		{name: "host wins", result: domain.PlayerOneWins, wantIDs: []string{"escrow", "host", "house"}, wantAmounts: []int64{-100, 75, 25}, wantRole: "host", wantHouse: 25},
		{name: "guest wins", result: domain.PlayerTwoWins, wantIDs: []string{"escrow", "guest", "house"}, wantAmounts: []int64{-100, 75, 25}, wantRole: "guest", wantHouse: 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, amounts, role, house := settlementEffects(domain.State{Result: tt.result}, []string{"host", "guest"}, "escrow")
			if !reflect.DeepEqual(ids, tt.wantIDs) || !reflect.DeepEqual(amounts, tt.wantAmounts) || role != tt.wantRole || house != tt.wantHouse {
				t.Fatalf("settlementEffects() = ids=%v amounts=%v role=%q house=%d", ids, amounts, role, house)
			}
			var total int64
			for _, amount := range amounts {
				total += amount
			}
			if total != 0 || -amounts[0] != application.RoundStake*2 {
				t.Fatalf("unbalanced effects amounts=%v total=%d", amounts, total)
			}
		})
	}
}
