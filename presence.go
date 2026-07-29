package portal

import (
	"encoding/json"

	"github.com/Jibaru/portal-go/wire"
)

// presenceTracker maintains the channel's presence state from the ready
// snapshot and subsequent deltas. Not goroutine-safe: the owning connection
// serialises access.
type presenceTracker struct {
	roster map[string]Participant
	order  []string // insertion order, so the roster renders stably
	kind   PresenceKind
	seeded bool
	count  int
	recent []json.RawMessage
}

func newPresenceTracker() *presenceTracker {
	return &presenceTracker{roster: map[string]Participant{}}
}

func toParticipant(p wire.PresenceParticipant) Participant {
	return Participant{ID: p.ID, Anon: p.Anon, Username: p.Username, Metadata: p.Metadata}
}

// seed applies the ready snapshot.
func (t *presenceTracker) seed(snapshot wire.PresenceSnapshot) {
	if snapshot.Mode == string(PresenceDetailed) {
		t.kind = PresenceDetailed
		t.roster = map[string]Participant{}
		t.order = nil
		for _, p := range snapshot.Participants {
			t.add(toParticipant(p))
		}
		t.count = snapshot.Count
	} else {
		t.kind = PresenceAggregate
		t.count = snapshot.Count
		t.recent = asRecent(snapshot.Recent)
	}
	t.seeded = true
}

// applyDelta applies a presence delta frame.
func (t *presenceTracker) applyDelta(frame *wire.PresenceFrame) {
	if frame.Mode == string(PresenceDetailed) {
		t.kind = PresenceDetailed
		for _, p := range frame.Joined {
			t.add(toParticipant(p))
		}
		for _, id := range frame.Left {
			t.remove(id)
		}
		t.count = frame.Count
	} else {
		t.kind = PresenceAggregate
		t.count = frame.Count
		t.recent = asRecent(frame.Recent)
	}
	t.seeded = true
}

// current is the public presence, or nil before any snapshot/delta.
func (t *presenceTracker) current() *Presence {
	if !t.seeded {
		return nil
	}
	if t.kind == PresenceDetailed {
		participants := make([]Participant, 0, len(t.order))
		for _, id := range t.order {
			participants = append(participants, t.roster[id])
		}
		return &Presence{Kind: PresenceDetailed, Participants: participants, Count: t.count}
	}
	return &Presence{Kind: PresenceAggregate, Count: t.count, Recent: t.recent}
}

func (t *presenceTracker) reset() {
	t.roster = map[string]Participant{}
	t.order = nil
	t.kind = ""
	t.seeded = false
	t.count = 0
	t.recent = nil
}

func (t *presenceTracker) add(p Participant) {
	if _, ok := t.roster[p.ID]; !ok {
		t.order = append(t.order, p.ID)
	}
	t.roster[p.ID] = p
}

func (t *presenceTracker) remove(id string) {
	if _, ok := t.roster[id]; !ok {
		return
	}
	delete(t.roster, id)
	for i, held := range t.order {
		if held == id {
			t.order = append(t.order[:i], t.order[i+1:]...)
			break
		}
	}
}

func asRecent(recent []wire.RawJSON) []json.RawMessage {
	if recent == nil {
		return []json.RawMessage{}
	}
	out := make([]json.RawMessage, len(recent))
	for i, r := range recent {
		out[i] = json.RawMessage(r)
	}
	return out
}
