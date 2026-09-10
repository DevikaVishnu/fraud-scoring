// Package heuristics scores an authorization with four hand-written rules.
//
// This is the explored alternative recorded in ADR-0007, not the decision path
// the rest of this repo uses. Everything here is a tuned threshold, which is
// exactly what ADR-0003 argues against and what ADR-0006 measured as
// net-harmful when a single such rule was allowed to decline on its own. The
// difference that makes this defensible is the routing: nothing here declines.
// A rule firing sends the authorization to a human queue, where a false
// positive costs review time rather than a turned-away cardholder.
//
// Thresholds come from the reconnaissance runs behind ADR-0002 and ADR-0006
// wherever the data can supply them, and are marked as chosen where it cannot.
package heuristics

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
)

// A Rule is one heuristic: a name, and the test it applies to derived signals.
type Rule struct {
	Name string
	// Why is the evidence or judgement behind the threshold, carried alongside
	// it so a review queue can show an operator what fired and on what basis.
	Why string
	// Points is what firing contributes to the total. Rules with better
	// measured lift are worth more.
	Points float64
	fires  func(Signals, Thresholds) bool
}

// Signals is what the rules read. It is the serving side's derived signal set,
// named rather than a bare vector so a rule cannot read the wrong slot.
type Signals struct {
	AmountUSD   float64
	GapSeconds  float64 // NaN when this is the card's first payment
	Velocity1h  float64
	Velocity24h float64
}

// Thresholds are the tuned numbers. They are configuration because they are
// tuned: ADR-0003's objection to thresholds does not stop applying just because
// these ones route rather than decline, so they are kept where the choice is
// visible.
type Thresholds struct {
	// RapidSuccessionSeconds: ADR-0006 measured gaps under 5s at 2.9% precision
	// against a 0.516% base rate -- roughly five times lift, the best of any
	// candidate it tried, and still far too weak to decline on.
	RapidSuccessionSeconds float64 `json:"rapid_succession_seconds"`

	// HourlyVelocityAt / DailyVelocityAt: ADR-0002 found the busiest card in
	// this data reaches 9 payments in an hour and 36 in a day. These sit below
	// those ceilings so the rules fire on the tail rather than never.
	HourlyVelocityAt float64 `json:"hourly_velocity_at"`
	DailyVelocityAt  float64 `json:"daily_velocity_at"`

	// LargeAmountUSD is chosen, not measured. No run in this repo established
	// an amount threshold; it is here because amount is the one signal whose
	// cost consequence is direct, and a large amount is worth an eye.
	LargeAmountUSD float64 `json:"large_amount_usd"`

	// ReviewAt is the total points at or above which an authorization is routed
	// to review instead of approved.
	ReviewAt float64 `json:"review_at"`
}

// Rules is the four heuristics, in the order they are reported.
//
// A card's first payment has no gap. It is NaN, and the rapid-succession rule
// must not fire on it: NaN comparisons are false in Go, which gives the right
// answer here, but the test is written explicitly so it stays right if the
// comparison is ever inverted.
var Rules = []Rule{
	{
		Name:   "rapid_succession",
		Why:    "payment follows the card's previous one almost immediately (ADR-0006: ~5x base rate)",
		Points: 40,
		fires: func(s Signals, t Thresholds) bool {
			if math.IsNaN(s.GapSeconds) {
				return false
			}
			return s.GapSeconds < t.RapidSuccessionSeconds
		},
	},
	{
		Name:   "hourly_velocity",
		Why:    "unusual number of payments on this card within the hour (ADR-0002: busiest card reaches 9)",
		Points: 30,
		fires: func(s Signals, t Thresholds) bool {
			return s.Velocity1h >= t.HourlyVelocityAt
		},
	},
	{
		Name:   "daily_velocity",
		Why:    "unusual number of payments on this card within the day (ADR-0002: busiest card reaches 36)",
		Points: 20,
		fires: func(s Signals, t Thresholds) bool {
			return s.Velocity24h >= t.DailyVelocityAt
		},
	},
	{
		Name:   "large_amount",
		Why:    "payment amount is large enough to be worth an eye (threshold chosen, not measured)",
		Points: 25,
		fires: func(s Signals, t Thresholds) bool {
			return s.AmountUSD >= t.LargeAmountUSD
		},
	},
}

// A Route is the topic an authorization is sent to.
type Route string

const (
	Approved Route = "approved"
	Review   Route = "review"
)

// An Assessment explains itself: the points, the route, and every rule that
// fired with the reason it fired. A review queue shows an operator this.
type Assessment struct {
	Points float64 `json:"points"`
	Route  Route   `json:"route"`
	Fired  []Fired `json:"fired"`
}

// Fired is one rule that matched.
type Fired struct {
	Rule   string  `json:"rule"`
	Why    string  `json:"why"`
	Points float64 `json:"points"`
}

// Assess runs every rule and routes on the total. Rules are independent: none
// short-circuits another, so the assessment always lists everything that fired
// rather than the first thing that did.
func (t Thresholds) Assess(s Signals) Assessment {
	assessment := Assessment{Route: Approved}
	for _, rule := range Rules {
		if !rule.fires(s, t) {
			continue
		}
		assessment.Points += rule.Points
		assessment.Fired = append(assessment.Fired, Fired{
			Rule: rule.Name, Why: rule.Why, Points: rule.Points,
		})
	}
	if assessment.Points >= t.ReviewAt {
		assessment.Route = Review
	}
	return assessment
}

// LoadThresholds reads the tuned numbers. A threshold that is not a positive
// number fails here rather than silently disabling a rule: a rule that can
// never fire is worse than no rule, because it looks like coverage.
func LoadThresholds(path string) (Thresholds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Thresholds{}, fmt.Errorf("loading thresholds %s: %w", path, err)
	}
	var thresholds Thresholds
	if err := json.Unmarshal(raw, &thresholds); err != nil {
		return Thresholds{}, fmt.Errorf("parsing thresholds %s: %w", path, err)
	}
	named := map[string]float64{
		"rapid_succession_seconds": thresholds.RapidSuccessionSeconds,
		"hourly_velocity_at":       thresholds.HourlyVelocityAt,
		"daily_velocity_at":        thresholds.DailyVelocityAt,
		"large_amount_usd":         thresholds.LargeAmountUSD,
		"review_at":                thresholds.ReviewAt,
	}
	for _, key := range sortedKeys(named) {
		if named[key] <= 0 {
			return Thresholds{}, fmt.Errorf("thresholds %s: %s is %v, which would stop the rule ever firing",
				path, key, named[key])
		}
	}
	if reachable := totalPoints(); thresholds.ReviewAt > reachable {
		return Thresholds{}, fmt.Errorf("thresholds %s: review_at is %v but every rule together scores %v, so nothing could ever route to review",
			path, thresholds.ReviewAt, reachable)
	}
	return thresholds, nil
}

func totalPoints() float64 {
	var total float64
	for _, rule := range Rules {
		total += rule.Points
	}
	return total
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
