package scoring_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/scoring"
)

// The fixture is the joint between the two languages. Python derives the
// signals and produces the calibrated risk scores; Go derives them again on the
// serving path and must agree. Nothing else forces the two implementations to
// stay in step, so this test failing at build time is the point: train/serve
// skew shows up here rather than in the decision mix months later.

const fixturePath = "../testdata/train_serve_fixture.json"

type fixture struct {
	SignalSet          []string `json:"signal_set"`
	RiskScoreTolerance float64  `json:"risk_score_tolerance"`
	Cases              []struct {
		Name          string `json:"name"`
		Authorization struct {
			TransactionID string  `json:"transaction_id"`
			Card          string  `json:"card"`
			EventTime     int64   `json:"event_time"`
			AmountUSD     float64 `json:"amount_usd"`
		} `json:"authorization"`
		// A null gap is a card's first payment, which is NaN on the serving
		// side; JSON has no NaN, so the pointer carries the distinction.
		Signals   map[string]*float64 `json:"signals"`
		RiskScore float64             `json:"risk_score"`
	} `json:"cases"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading the fixture: %v (regenerate it with `make train`)", err)
	}
	var loaded fixture
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("parsing the fixture: %v", err)
	}
	if len(loaded.Cases) == 0 {
		t.Fatal("the fixture has no cases")
	}
	return loaded
}

func TestServingDerivesTheSameSignalsAsTraining(t *testing.T) {
	loaded := loadFixture(t)
	scorer := newScorer(t, config(t))

	for _, testCase := range loaded.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			decision := scorer.Score(context.Background(), scoring.Authorization{
				TransactionID: testCase.Authorization.TransactionID,
				Card:          testCase.Authorization.Card,
				EventTime:     time.Unix(testCase.Authorization.EventTime, 0).UTC(),
				AmountUSD:     testCase.Authorization.AmountUSD,
			})
			if decision.Degraded {
				t.Fatalf("scoring degraded: %s", decision.DegradedReason)
			}

			// Signals are asserted exactly. An off-by-one in a window boundary
			// or a difference in the ordering tie-break must not be able to
			// hide inside a tolerance.
			for name, want := range testCase.Signals {
				got, present := decision.Signals[name]
				if !present {
					t.Errorf("%s: the serving side derived no such signal", name)
					continue
				}
				switch {
				case want == nil && !math.IsNaN(got):
					t.Errorf("%s: got %v, want no value", name, got)
				case want != nil && got != *want:
					t.Errorf("%s: got %v, want %v", name, got, *want)
				}
			}

			// Risk scores are asserted within a stated tolerance, because two
			// runtimes summing the same trees will not agree bit for bit.
			if diff := math.Abs(decision.RiskScore - testCase.RiskScore); diff > loaded.RiskScoreTolerance {
				t.Errorf("risk score: got %.17g, want %.17g (differs by %g, tolerance %g)",
					decision.RiskScore, testCase.RiskScore, diff, loaded.RiskScoreTolerance)
			}
		})
	}
}

func TestFixturePinsTheCasesThatAreEasyToGetWrong(t *testing.T) {
	loaded := loadFixture(t)

	named := map[string]bool{}
	for _, testCase := range loaded.Cases {
		named[testCase.Name] = true
	}
	for _, required := range []string{
		"boundary-first-payment-on-card",
		"boundary-same-second-tie-break",
		"boundary-exactly-on-velocity_1h",
		"boundary-exactly-on-velocity_24h",
	} {
		if !named[required] {
			t.Errorf("the fixture has no %q case", required)
		}
	}
}

func TestFixtureCoversTheWholeSignalSet(t *testing.T) {
	loaded := loadFixture(t)
	serving := scoring.SignalSet()

	if len(loaded.SignalSet) != len(serving) {
		t.Fatalf("the training side reads %v, the serving side reads %v",
			loaded.SignalSet, serving)
	}
	for i, name := range serving {
		if loaded.SignalSet[i] != name {
			t.Fatalf("signal %d: the training side reads %q, the serving side reads %q",
				i, loaded.SignalSet[i], name)
		}
	}
}
