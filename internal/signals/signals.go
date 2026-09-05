// Package signals derives the signals the model reads from a card's history.
//
// Every window and every gap is measured against event time -- the timestamp
// carried on the authorization -- never against the clock at which scoring
// happens, so a replayed authorization produces the values it produced live.
//
// The training side states the same definitions in training/signals.py. Nothing
// makes the two agree except the train/serve fixture, which is why that fixture
// is a build artifact rather than a belief; see scoring's fixture test.
package signals

import (
	"math"
	"sort"
)

// Set is the signals the model reads, in the order the model reads them. This
// is the one place the set is stated on the serving side; adding a signal is an
// edit here, in Derive, and in training/signals.py.
var Set = []string{"amount", "gap_seconds", "velocity_1h", "velocity_24h"}

// Velocity counter windows, in seconds. A payment sitting exactly Window
// seconds before the authorization is counted: the lower bound is inclusive.
var velocityWindows = []struct {
	Name   string
	Window int64
}{
	{"velocity_1h", 3600},
	{"velocity_24h", 86400},
}

// A Payment is one entry in a card's history, identified well enough to be
// ordered against the others.
type Payment struct {
	TransactionID string
	EventTime     int64 // unix seconds
}

// A Timeline is one card's payments in key order: event time, then transaction
// id. Whole-second timestamp resolution puts genuinely distinct payments at the
// same event time, so the transaction id is what leaves "the previous payment"
// defined (ADR-0006).
type Timeline []Payment

func (t Timeline) Len() int      { return len(t) }
func (t Timeline) Swap(i, j int) { t[i], t[j] = t[j], t[i] }
func (t Timeline) Less(i, j int) bool {
	if t[i].EventTime != t[j].EventTime {
		return t[i].EventTime < t[j].EventTime
	}
	return t[i].TransactionID < t[j].TransactionID
}

// Derive assembles every signal in Set for one authorization.
//
// Only payments strictly before the authorization in key order are read, so a
// signal never contains information from its own outcome, and history is that
// one card's own, so interleaved traffic across cards cannot contaminate a
// counter. A missing gap -- a card's first payment -- is NaN rather than a
// number the model would read as a very short gap.
func Derive(history Timeline, auth Payment, amountUSD float64) map[string]float64 {
	predecessors := history[:sort.Search(len(history), func(i int) bool {
		return !before(history[i], auth)
	})]

	derived := map[string]float64{
		"amount":      amountUSD,
		"gap_seconds": math.NaN(),
	}
	if n := len(predecessors); n > 0 {
		derived["gap_seconds"] = float64(auth.EventTime - predecessors[n-1].EventTime)
	}
	for _, counter := range velocityWindows {
		derived[counter.Name] = float64(len(predecessors) - firstInWindow(predecessors, auth.EventTime, counter.Window))
	}
	return derived
}

// firstInWindow is the index of the earliest payment within `window` seconds
// before `at`, inclusive of the boundary itself.
func firstInWindow(predecessors Timeline, at, window int64) int {
	return sort.Search(len(predecessors), func(i int) bool {
		return predecessors[i].EventTime >= at-window
	})
}

func before(a, b Payment) bool {
	if a.EventTime != b.EventTime {
		return a.EventTime < b.EventTime
	}
	return a.TransactionID < b.TransactionID
}

// Vector lays derived signals out in Set order, which is the order the model
// was trained to read them in.
func Vector(derived map[string]float64) []float64 {
	vector := make([]float64, len(Set))
	for i, name := range Set {
		vector[i] = derived[name]
	}
	return vector
}
