package scoring_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/scoring"
	"github.com/parquet-go/parquet-go"
)

// Everything here goes through the entry point, because the entry point is the
// only public surface. That is deliberate: a test that could reach inside would
// end up pinning how the answer is computed rather than what it is.

func config(t *testing.T) scoring.Config {
	t.Helper()
	config := scoring.DefaultConfig()
	config.ModelPath = "../artifacts/model.txt"
	config.CalibrationPath = "../artifacts/calibration.json"
	config.CostsPath = "../config/costs.json"
	config.PaymentHistory = []string{"../data/fixture_payments.csv"}
	config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return config
}

func newScorer(t *testing.T, c scoring.Config) *scoring.Scorer {
	t.Helper()
	scorer, err := scoring.New(c)
	if err != nil {
		t.Fatalf("loading the scorer: %v", err)
	}
	t.Cleanup(func() {
		if err := scorer.Close(); err != nil {
			t.Errorf("closing the scorer: %v", err)
		}
	})
	return scorer
}

// syntheticScorer scores against a hand-written history small enough to reason
// about, so the window and ordering rules can be stated as arithmetic.
func syntheticScorer(t *testing.T) *scoring.Scorer {
	t.Helper()
	c := config(t)
	c.PaymentHistory = []string{"../testdata/synthetic_payments.csv"}
	return newScorer(t, c)
}

func authorization(id, card string, at int64, amount float64) scoring.Authorization {
	return scoring.Authorization{
		TransactionID: id,
		Card:          card,
		EventTime:     time.Unix(at, 0).UTC(),
		AmountUSD:     amount,
	}
}

func costs(t *testing.T) map[string]float64 {
	t.Helper()
	raw, err := os.ReadFile("../config/costs.json")
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]any
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	numbers := map[string]float64{}
	for key, value := range loaded {
		if number, ok := value.(float64); ok {
			numbers[key] = number
		}
	}
	return numbers
}

func TestOneAuthorizationInOneDecisionOut(t *testing.T) {
	scorer := syntheticScorer(t)

	decision := scorer.Score(context.Background(),
		authorization("t1", "SYNTH-A", 1000003700, 42.50))

	switch decision.Verdict {
	case scoring.Approve, scoring.Decline, scoring.Challenge:
	default:
		t.Errorf("verdict %q is not one of the three decisions", decision.Verdict)
	}
	if decision.RiskScore < 0 || decision.RiskScore > 1 {
		t.Errorf("risk score %v is not a probability", decision.RiskScore)
	}
	if decision.Authorization.TransactionID != "t1" {
		t.Errorf("the decision does not carry its authorization back")
	}
}

func TestACardsFirstPaymentIsScorable(t *testing.T) {
	scorer := syntheticScorer(t)

	decision := scorer.Score(context.Background(),
		authorization("first", "SYNTH-NEVER-SEEN", 1000003700, 42.50))

	if decision.Degraded {
		t.Fatalf("a card with no history degraded scoring: %s", decision.DegradedReason)
	}
	if !math.IsNaN(decision.Signals["gap_seconds"]) {
		t.Errorf("gap: got %v, want no value", decision.Signals["gap_seconds"])
	}
	for _, counter := range []string{"velocity_1h", "velocity_24h"} {
		if decision.Signals[counter] != 0 {
			t.Errorf("%s: got %v, want 0", counter, decision.Signals[counter])
		}
	}
}

func TestVelocityCountersIncludeThePaymentOnTheWindowBoundary(t *testing.T) {
	scorer := syntheticScorer(t)

	// a1 sits at 1000000000, exactly one hour before this authorization.
	onBoundary := scorer.Score(context.Background(),
		authorization("a2", "SYNTH-A", 1000003600, 10))
	if got := onBoundary.Signals["velocity_1h"]; got != 1 {
		t.Errorf("a payment exactly on the boundary: velocity_1h got %v, want 1", got)
	}

	// One second later a1 has fallen out of the window, leaving a2 and a3.
	pastBoundary := scorer.Score(context.Background(),
		authorization("aaa", "SYNTH-A", 1000003601, 10))
	if got := pastBoundary.Signals["velocity_1h"]; got != 2 {
		t.Errorf("a payment one second past the boundary: velocity_1h got %v, want 2", got)
	}
}

func TestPaymentsSharingASecondAreOrderedByTransactionID(t *testing.T) {
	scorer := syntheticScorer(t)
	const sharedSecond = 1000003601 // a3 already sits here

	// "a25" sorts before "a3", so a3 is not yet a predecessor.
	before := scorer.Score(context.Background(),
		authorization("a25", "SYNTH-A", sharedSecond, 10))
	if got := before.Signals["gap_seconds"]; got != 1 {
		t.Errorf("ordering before a3: gap got %v, want 1 (the gap back to a2)", got)
	}

	// "a9" sorts after "a3", so a3 is a predecessor and the gap collapses to zero.
	after := scorer.Score(context.Background(),
		authorization("a9", "SYNTH-A", sharedSecond, 10))
	if got := after.Signals["gap_seconds"]; got != 0 {
		t.Errorf("ordering after a3: gap got %v, want 0", got)
	}
}

func TestSignalsReadOnlyThisCardsOwnHistory(t *testing.T) {
	scorer := syntheticScorer(t)

	// SYNTH-A has three payments before this moment and SYNTH-B has three of
	// its own; neither card may see the other's.
	onB := scorer.Score(context.Background(),
		authorization("bbb", "SYNTH-B", 1000003700, 10))

	if got := onB.Signals["velocity_24h"]; got != 3 {
		t.Errorf("velocity_24h on SYNTH-B: got %v, want 3", got)
	}
	if got := onB.Signals["gap_seconds"]; got != 200 {
		t.Errorf("gap on SYNTH-B: got %v, want 200 (back to b3)", got)
	}
}

func TestSignalsNeverReadTheAuthorizationOrAnythingAfterIt(t *testing.T) {
	scorer := syntheticScorer(t)

	// Scored at the moment of a1, the card's very first payment: a1 itself and
	// everything after it are invisible.
	decision := scorer.Score(context.Background(),
		authorization("a1", "SYNTH-A", 1000000000, 10))

	if !math.IsNaN(decision.Signals["gap_seconds"]) {
		t.Errorf("gap: got %v, want no value", decision.Signals["gap_seconds"])
	}
	if got := decision.Signals["velocity_24h"]; got != 0 {
		t.Errorf("velocity_24h: got %v, want 0", got)
	}
}

func TestTheDecisionIsTheCheapestExpectedCost(t *testing.T) {
	scorer := syntheticScorer(t)
	inputs := costs(t)

	for _, amount := range []float64{1, 25, 250, 2500, 25000} {
		decision := scorer.Score(context.Background(),
			authorization("t", "SYNTH-A", 1000003700, amount))

		// Recomputed by hand from the decision's own inputs, which is the
		// property that makes a decision explainable.
		fraud, legitimate := decision.RiskScore, 1-decision.RiskScore
		falseDecline := inputs["false_decline_fixed"] + inputs["false_decline_rate"]*amount
		want := map[scoring.Verdict]float64{
			scoring.Approve: fraud * (amount*inputs["fraud_loss_multiple"] + inputs["chargeback_fee"]),
			scoring.Decline: legitimate * falseDecline,
			scoring.Challenge: inputs["challenge_cost"] +
				legitimate*inputs["challenge_abandon_rate"]*falseDecline,
		}

		cheapest := scoring.Approve
		for verdict, cost := range want {
			if cost < want[cheapest] {
				cheapest = verdict
			}
			if got := decision.ExpectedCosts[verdict]; math.Abs(got-cost) > 1e-9 {
				t.Errorf("$%.2f: expected cost of %s: got %v, want %v", amount, verdict, got, cost)
			}
		}
		if decision.Verdict != cheapest {
			t.Errorf("$%.2f: got %s, but %s is cheaper (%v)",
				amount, decision.Verdict, cheapest, want)
		}
	}
}

func TestTheCutoffMovesWithThePaymentAmount(t *testing.T) {
	scorer := syntheticScorer(t)

	// The same card at the same moment, so the only thing that changes is what
	// is at stake. No band is tuned anywhere; the amount is inside the cost of
	// a missed fraud, and that is enough to move the answer.
	small := scorer.Score(context.Background(), authorization("t", "SYNTH-A", 1000003700, 1))
	large := scorer.Score(context.Background(), authorization("t", "SYNTH-A", 1000003700, 100000))

	if small.RiskScore != large.RiskScore {
		t.Logf("note: the amount is also a signal, so the risk scores differ (%v vs %v)",
			small.RiskScore, large.RiskScore)
	}
	if small.Verdict == large.Verdict {
		t.Errorf("$1 and $100,000 both got %s; the cutoff is not moving with the amount",
			small.Verdict)
	}
}

func TestChallengeIsReachable(t *testing.T) {
	loaded := loadFixture(t)
	scorer := newScorer(t, config(t))

	// Challenge has to be a real option rather than dead arithmetic. The
	// fixture's first-payment case is a large payment on a card with no
	// history, which is exactly the shape that prices it cheapest.
	for _, testCase := range loaded.Cases {
		decision := scorer.Score(context.Background(), authorization(
			testCase.Authorization.TransactionID,
			testCase.Authorization.Card,
			testCase.Authorization.EventTime,
			testCase.Authorization.AmountUSD,
		))
		if decision.Verdict == scoring.Challenge {
			return
		}
	}
	t.Error("no fixture authorization reaches challenge")
}

func TestAFailureInsideTheSystemStillAnswers(t *testing.T) {
	scorer := syntheticScorer(t)
	inputs := costs(t)

	// A cancelled context stands in for any failure on the way to a score:
	// there is no time left to compute one.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	decision := scorer.Score(cancelled, authorization("t", "SYNTH-A", 1000003700, 250))

	if !decision.Degraded {
		t.Fatal("the decision does not say it degraded")
	}
	if decision.RiskScore != inputs["base_fraud_rate"] {
		t.Errorf("risk score: got %v, want the base fraud rate %v",
			decision.RiskScore, inputs["base_fraud_rate"])
	}
	// The same formula, with the prior in place of a score. There is no second
	// policy to maintain.
	fallback := scoring.Verdict("")
	for verdict, cost := range decision.ExpectedCosts {
		if fallback == "" || cost < decision.ExpectedCosts[fallback] {
			fallback = verdict
		}
	}
	if decision.Verdict != fallback {
		t.Errorf("degraded verdict %s is not the cheapest of %v",
			decision.Verdict, decision.ExpectedCosts)
	}
}

func TestMissingArtifactsFailAtLoad(t *testing.T) {
	for name, breakIt := range map[string]func(*scoring.Config){
		"model":       func(c *scoring.Config) { c.ModelPath = "../artifacts/no-such-model.txt" },
		"calibration": func(c *scoring.Config) { c.CalibrationPath = "../artifacts/no-such-table.json" },
		"costs":       func(c *scoring.Config) { c.CostsPath = "../config/no-such-costs.json" },
		"payments":    func(c *scoring.Config) { c.PaymentHistory = []string{"../data/no-such-payments.csv"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := config(t)
			breakIt(&c)
			scorer, err := scoring.New(c)
			if err == nil {
				scorer.Close()
				t.Fatal("a missing artifact loaded without complaint")
			}
		})
	}
}

func TestMalformedCalibrationFailsAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calibration.json")
	if err := os.WriteFile(path, []byte(`{"kind":"isotonic","x":[0.1,0.2],"y":[7.0,9.0]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := config(t)
	c.CalibrationPath = path

	scorer, err := scoring.New(c)
	if err == nil {
		scorer.Close()
		t.Fatal("a calibration table whose outputs are not probabilities loaded without complaint")
	}
}

func TestEveryScoredAuthorizationIsPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.parquet")
	c := config(t)
	c.PaymentHistory = []string{"../testdata/synthetic_payments.csv"}
	c.HistoryPath = path
	scorer, err := scoring.New(c)
	if err != nil {
		t.Fatal(err)
	}

	auth := authorization("audit-me", "SYNTH-A", 1000003700, 99.99)
	decision := scorer.Score(context.Background(), auth)
	if err := scorer.Close(); err != nil {
		t.Fatalf("closing the scorer: %v", err)
	}

	type record struct {
		TransactionID string             `parquet:"transaction_id"`
		Card          string             `parquet:"card"`
		EventTime     int64              `parquet:"event_time"`
		AmountUSD     float64            `parquet:"amount_usd"`
		Signals       map[string]float64 `parquet:"signals"`
		RiskScore     float64            `parquet:"risk_score"`
		Verdict       string             `parquet:"verdict"`
	}
	rows, err := parquet.ReadFile[record](path)
	if err != nil {
		t.Fatalf("reading history back in bulk: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history holds %d records, want 1", len(rows))
	}

	// The record has to explain itself without anything being re-derived.
	got := rows[0]
	if got.TransactionID != auth.TransactionID || got.Card != auth.Card {
		t.Errorf("history does not identify the authorization: %+v", got)
	}
	if got.EventTime != auth.EventTime.Unix() {
		t.Errorf("event time: got %v, want %v", got.EventTime, auth.EventTime.Unix())
	}
	if got.RiskScore != decision.RiskScore || got.Verdict != string(decision.Verdict) {
		t.Errorf("history disagrees with the decision returned: %+v", got)
	}
	for _, name := range scoring.SignalSet() {
		if want := decision.Signals[name]; got.Signals[name] != want &&
			!(math.IsNaN(want) && math.IsNaN(got.Signals[name])) {
			t.Errorf("history signal %s: got %v, want %v", name, got.Signals[name], want)
		}
	}
}

func TestPersistenceDoesNotChangeTheDecision(t *testing.T) {
	auth := authorization("t", "SYNTH-A", 1000003700, 250)

	withoutHistory := syntheticScorer(t).Score(context.Background(), auth)

	c := config(t)
	c.PaymentHistory = []string{"../testdata/synthetic_payments.csv"}
	c.HistoryPath = filepath.Join(t.TempDir(), "history.parquet")
	withHistory := newScorer(t, c).Score(context.Background(), auth)

	if withoutHistory.Verdict != withHistory.Verdict ||
		withoutHistory.RiskScore != withHistory.RiskScore {
		t.Errorf("the history path changed the decision: %v then %v",
			withoutHistory.Verdict, withHistory.Verdict)
	}
}
