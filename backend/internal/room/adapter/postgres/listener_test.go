package postgres

import "testing"

func TestParseRoomHintAcceptsOnlyBoundedMetadata(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{name: "room and round", payload: "ABC234:7", valid: true},
		{name: "missing round", payload: "ABC234", valid: false},
		{name: "zero round", payload: "ABC234:0", valid: false},
		{name: "unsafe code", payload: "room token:1", valid: false},
		{name: "oversized", payload: string(make([]byte, 129)), valid: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hint, err := parseRoomHint(tt.payload)
			if tt.valid && (err != nil || hint.RoomCode != "ABC234" || hint.Round != 7) {
				t.Fatalf("hint = %#v, error = %v", hint, err)
			}
			if !tt.valid && err == nil {
				t.Fatalf("accepted hint %#v", hint)
			}
		})
	}
}
