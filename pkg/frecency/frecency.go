// Package frecency ports crates/frecency: event ingestion, the aggregate
// score computation, and the polling aggregator worker that folds
// frecency_events into frecency_aggregates.
//
// Frecency = 0.7 * log2(event_count + 2)  +  0.3 * Σ exp(-0.1*Δh) * weight
// where weight is per action: open=+2, ping=0, close=−1. recent_events is
// capped at the 10 most recent TimestampWeights.
package frecency

import (
	"math"
	"time"
)

// Action weights, matching frecency::domain::models::WeightForAction.
const (
	ActionOpen  = "open"
	ActionPing  = "ping"
	ActionClose = "close"
)

// Weight returns the score contribution of an action.
func Weight(action string) float64 {
	switch action {
	case ActionOpen:
		return 2.0
	case ActionPing:
		return 0.0
	case ActionClose:
		return -1.0
	}
	return 0.0
}

const (
	maxRecentEvents  = 10
	recencyDecayRate = 0.1 // per hour
	frequencyPercent = 0.7
)

// TimestampWeight is one entry of Aggregate.RecentEvents.
type TimestampWeight struct {
	Timestamp time.Time `json:"timestamp"`
	Weight    float64   `json:"weight"`
}

// Aggregate is a frecency_aggregates row keyed by (user, entity_type,
// entity_id).
type Aggregate struct {
	UserID      string
	EntityType  string
	EntityID    string
	EventCount  int
	Score       float64
	FirstEvent  time.Time
	RecentEvent []TimestampWeight
}

// NewAggregate builds the initial aggregate from the first event (ports
// new_from_initial_action_and_user_id).
func NewAggregate(userID, entityType, entityID string, ev Event, now time.Time) Aggregate {
	a := Aggregate{
		UserID:     userID,
		EntityType: entityType,
		EntityID:   entityID,
		FirstEvent: ev.Timestamp,
	}
	a.Append(ev, now)
	return a
}

// Append folds one event into the aggregate (ports append_event_inner).
func (a *Aggregate) Append(ev Event, now time.Time) {
	a.EventCount++
	// push front, cap at maxRecentEvents
	a.RecentEvent = append([]TimestampWeight{{Timestamp: ev.Timestamp, Weight: Weight(ev.Action)}}, a.RecentEvent...)
	if len(a.RecentEvent) > maxRecentEvents {
		a.RecentEvent = a.RecentEvent[:maxRecentEvents]
	}
	a.Score = a.Frecency(now)
}

func (a *Aggregate) frequency() float64 {
	return math.Log2(float64(a.EventCount) + 2.0)
}

func (a *Aggregate) recency(now time.Time) float64 {
	var acc float64
	for _, e := range a.RecentEvent {
		hours := now.Sub(e.Timestamp).Hours()
		if hours < 0 {
			continue
		}
		acc += math.Exp(-recencyDecayRate*hours) * e.Weight
	}
	return acc
}

// Frecency computes the aggregate score at `now`.
func (a *Aggregate) Frecency(now time.Time) float64 {
	return frequencyPercent*a.frequency() + (1.0-frequencyPercent)*a.recency(now)
}

// Event is a single frecency_events row being processed.
type Event struct {
	ID         int64
	UserID     string
	EntityType string
	EntityID   string
	Action     string // event_type column: open | ping | close
	Timestamp  time.Time
}
