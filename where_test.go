package portal

import "testing"

func TestWhereMatching(t *testing.T) {
	record := map[string]any{
		"type":      "mention",
		"channelId": "room-7",
		"read":      false,
		"priority":  float64(3),
		"label":     "beta",
	}
	cases := []struct {
		name  string
		where Where
		want  bool
	}{
		{"eq match", Where{"type": {Eq: []any{"mention"}}}, true},
		{"eq miss", Where{"type": {Eq: []any{"ticket.assigned"}}}, false},
		{"eq multi", Where{"type": {Eq: []any{"a", "mention"}}}, true},
		{"neq", Where{"type": {Neq: []any{"mention"}}}, false},
		{"in", Where{"channelId": {In: []any{"room-7", "room-8"}}}, true},
		{"gt number", Where{"priority": {Gt: 2}}, true},
		{"gt number int literal", Where{"priority": {Gt: 3}}, false},
		{"lt string", Where{"label": {Lt: "gamma"}}, true},
		{"bool eq", Where{"read": {Eq: []any{false}}}, true},
		{"missing field", Where{"nope": {Eq: []any{"x"}}}, false},
		{"ordered type mismatch", Where{"label": {Gt: 5}}, false},
		{"and semantics", Where{"type": {Eq: []any{"mention"}}, "read": {Eq: []any{true}}}, false},
	}
	for _, tc := range cases {
		if got := matchesWhere(record, tc.where); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecodeJWTClaims(t *testing.T) {
	// {"sub":"anon_42","exp":2000000000} — base64url, no padding.
	token := "eyJhbGciOiJub25lIn0.eyJzdWIiOiJhbm9uXzQyIiwiZXhwIjoyMDAwMDAwMDAwfQ.sig"
	claims := decodeJWTClaims(token)
	if claims.sub != "anon_42" {
		t.Errorf("sub mismatch: %q", claims.sub)
	}
	if claims.exp != 2000000000 {
		t.Errorf("exp mismatch: %d", claims.exp)
	}
	if got := decodeJWTClaims("garbage"); got.sub != "" || got.exp != 0 {
		t.Errorf("garbage token should decode empty, got %+v", got)
	}
}
