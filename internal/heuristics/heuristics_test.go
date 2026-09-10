package heuristics

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

// tuned is the shipped configuration, so the tests exercise the numbers that
// actually run rather than convenient ones.
var tuned = Thresholds{
	RapidSuccessionSeconds: 5,
	HourlyVelocityAt:       5,
	DailyVelocityAt:        20,
	LargeAmountUSD:         1000,
	ReviewAt:               40,
}

// ordinary is an authorization that fires nothing.
var ordinary = Signals{AmountUSD: 50, GapSeconds: 3600, Velocity1h: 1, Velocity24h: 3}

func TestAnOrdinaryAuthorizationFiresNothingAndIsApproved(t *testing.T) {
	got := tuned.Assess(ordinary)
	if got.Route != Approved {
		t.Errorf("route = %q, want %q", got.Route, Approved)
	}
	if got.Points != 0 || len(got.Fired) != 0 {
		t.Errorf("points = %v, fired = %v, want nothing", got.Points, got.Fired)
	}
}

func TestEachRuleFiresOnItsOwnSignal(t *testing.T) {
	for _, test := range []struct {
		rule    string
		signals Signals
	}{
		{"rapid_succession", Signals{AmountUSD: 50, GapSeconds: 4, Velocity1h: 1, Velocity24h: 3}},
		{"hourly_velocity", Signals{AmountUSD: 50, GapSeconds: 3600, Velocity1h: 5, Velocity24h: 3}},
		{"daily_velocity", Signals{AmountUSD: 50, GapSeconds: 3600, Velocity1h: 1, Velocity24h: 20}},
		{"large_amount", Signals{AmountUSD: 1000, GapSeconds: 3600, Velocity1h: 1, Velocity24h: 3}},
	} {
		t.Run(test.rule, func(t *testing.T) {
			fired := tuned.Assess(test.signals).Fired
			if len(fired) != 1 || fired[0].Rule != test.rule {
				t.Fatalf("fired %v, want exactly %s", fired, test.rule)
			}
		})
	}
}

// Every rule uses >= or < against its threshold, so the boundary value itself
// is inside the rule. Same convention as the velocity windows in
// internal/signals, and worth pinning for the same reason.
func TestTheThresholdValueItselfIsInsideTheRule(t *testing.T) {
	onBoundary := Signals{AmountUSD: 1000, GapSeconds: 5, Velocity1h: 5, Velocity24h: 20}
	fired := map[string]bool{}
	for _, f := range tuned.Assess(onBoundary).Fired {
		fired[f.Rule] = true
	}
	// The velocity and amount rules include their boundary; the gap rule is
	// strictly-under, so exactly-5s must not fire it.
	for _, rule := range []string{"hourly_velocity", "daily_velocity", "large_amount"} {
		if !fired[rule] {
			t.Errorf("%s did not fire on its boundary value", rule)
		}
	}
	if fired["rapid_succession"] {
		t.Error("rapid_succession fired at exactly the threshold; it is strictly-under")
	}
}

// A card's first payment has no previous payment, so it has no gap. NaN must
// not read as a very short one -- that would route every new card to review.
func TestACardsFirstPaymentDoesNotFireRapidSuccession(t *testing.T) {
	first := Signals{AmountUSD: 50, GapSeconds: math.NaN(), Velocity1h: 0, Velocity24h: 0}
	got := tuned.Assess(first)
	if got.Route != Approved {
		t.Fatalf("route = %q, want %q; fired %v", got.Route, Approved, got.Fired)
	}
}

func TestRulesAccumulateAndAreAllReported(t *testing.T) {
	everything := Signals{AmountUSD: 5000, GapSeconds: 1, Velocity1h: 9, Velocity24h: 36}
	got := tuned.Assess(everything)
	if len(got.Fired) != len(Rules) {
		t.Fatalf("fired %d rules, want all %d", len(got.Fired), len(Rules))
	}
	var want float64
	for _, rule := range Rules {
		want += rule.Points
	}
	if got.Points != want {
		t.Errorf("points = %v, want %v", got.Points, want)
	}
	if got.Route != Review {
		t.Errorf("route = %q, want %q", got.Route, Review)
	}
}

// review_at is the only thing that decides the route. A single strong rule is
// enough at the shipped value; below it, points accumulate without routing.
func TestRoutingHappensAtReviewAtAndNowhereElse(t *testing.T) {
	justUnder := tuned
	justUnder.ReviewAt = 21 // daily_velocity alone (20) no longer reaches it
	onlyDaily := Signals{AmountUSD: 50, GapSeconds: 3600, Velocity1h: 1, Velocity24h: 20}

	if got := justUnder.Assess(onlyDaily); got.Route != Approved {
		t.Errorf("20 points against review_at 21: route = %q, want %q", got.Route, Approved)
	}
	justAt := justUnder
	justAt.ReviewAt = 20
	if got := justAt.Assess(onlyDaily); got.Route != Review {
		t.Errorf("20 points against review_at 20: route = %q, want %q", got.Route, Review)
	}
}

func TestTheShippedThresholdsLoad(t *testing.T) {
	got, err := LoadThresholds(filepath.Join("..", "..", "config", "thresholds.json"))
	if err != nil {
		t.Fatalf("loading the shipped thresholds: %v", err)
	}
	if got != tuned {
		t.Errorf("shipped thresholds = %+v, but these tests assert against %+v", got, tuned)
	}
}

// A threshold that stops a rule ever firing is worse than no rule, because it
// looks like coverage. It must fail at load.
func TestUnusableThresholdsFailAtLoad(t *testing.T) {
	for _, test := range []struct{ name, json string }{
		{"a zero threshold", `{"rapid_succession_seconds":0,"hourly_velocity_at":5,"daily_velocity_at":20,"large_amount_usd":1000,"review_at":40}`},
		{"a negative threshold", `{"rapid_succession_seconds":5,"hourly_velocity_at":-1,"daily_velocity_at":20,"large_amount_usd":1000,"review_at":40}`},
		{"a missing threshold", `{"rapid_succession_seconds":5,"hourly_velocity_at":5,"daily_velocity_at":20,"review_at":40}`},
		{"an unreachable review_at", `{"rapid_succession_seconds":5,"hourly_velocity_at":5,"daily_velocity_at":20,"large_amount_usd":1000,"review_at":9999}`},
		{"not json at all", `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thresholds.json")
			if err := os.WriteFile(path, []byte(test.json), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadThresholds(path); err == nil {
				t.Error("loaded without error, want a failure at load")
			}
		})
	}
}

func TestAMissingThresholdsFileFailsAtLoad(t *testing.T) {
	if _, err := LoadThresholds(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("loaded without error, want a failure at load")
	}
}
