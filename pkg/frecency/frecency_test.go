package frecency

import (
	"math"
	"testing"
	"time"
)

func ev(userID, et, eid, action string, ts time.Time) Event {
	return Event{UserID: userID, EntityType: et, EntityID: eid, Action: action, Timestamp: ts}
}

func TestWeights(t *testing.T) {
	if Weight(ActionOpen) != 2.0 || Weight(ActionPing) != 0.0 || Weight(ActionClose) != -1.0 {
		t.Fatal("weights differ from Rust WeightForAction")
	}
}

// Port of the Rust formula: score = 0.7*log2(n+2) + 0.3*Σ e^(−0.1h)·w.
func TestFrecencyMatchesFormula(t *testing.T) {
	now := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	a := NewAggregate("macro|u@x.io", "channel", "c1", ev("macro|u@x.io", "channel", "c1", ActionOpen, now.Add(-2*time.Hour)), now)
	a.Append(ev("macro|u@x.io", "channel", "c1", ActionOpen, now), now)
	a.Append(ev("macro|u@x.io", "channel", "c1", ActionClose, now), now)

	wantFreq := 0.7 * math.Log2(3+2)
	// recent events (push-front): close@now, open@now, open@-2h
	wantRec := 0.3 * (math.Exp(0)*-1.0 + math.Exp(0)*2.0 + math.Exp(-0.1*2)*2.0)
	if got, want := a.Score, wantFreq+wantRec; math.Abs(got-want) > 1e-9 {
		t.Fatalf("score %v, want %v", got, want)
	}
	if a.EventCount != 3 {
		t.Fatalf("event_count %d", a.EventCount)
	}
}

func TestRecentEventsCapAt10(t *testing.T) {
	now := time.Now()
	a := NewAggregate("u", "channel", "c", ev("u", "channel", "c", ActionOpen, now), now)
	for i := 0; i < 20; i++ {
		a.Append(ev("u", "channel", "c", ActionOpen, now), now)
	}
	if len(a.RecentEvent) != maxRecentEvents {
		t.Fatalf("recent_events len %d, want %d", len(a.RecentEvent), maxRecentEvents)
	}
	// Most recent is first.
	if !a.RecentEvent[0].Timestamp.Equal(now) {
		t.Fatal("recent_events not push-front ordered")
	}
}

func TestFutureEventsIgnoredInRecency(t *testing.T) {
	now := time.Now()
	a := NewAggregate("u", "channel", "c", ev("u", "channel", "c", ActionOpen, now.Add(time.Hour)), now)
	// delta_hours < 0 → contributes 0 (Rust skip).
	want := 0.7*math.Log2(1+2) + 0.3*0.0
	if math.Abs(a.Score-want) > 1e-9 {
		t.Fatalf("score %v, want %v", a.Score, want)
	}
}
