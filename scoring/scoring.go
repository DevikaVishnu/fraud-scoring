// Package scoring is the entry point: one authorization in, one decision out.
//
// It is the only public surface this module has. Everything the decision is
// made of -- deriving signals from the card's history, evaluating the model,
// calibrating its output, comparing expected costs, persisting the result --
// lives under internal/, so a caller and a test can reach behaviour and cannot
// reach the parts. The shape here is the shape a request handler would call,
// so putting a service in front of it later is a wrapping job.
package scoring

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/internal/decisioning"
	"github.com/DevikaVishnu/fraud-scoring/internal/history"
	"github.com/DevikaVishnu/fraud-scoring/internal/risk"
	"github.com/DevikaVishnu/fraud-scoring/internal/signals"
)

// An Authorization is the unit of work: a request from the card network to
// approve or decline a card payment, held open under a hard deadline.
type Authorization struct {
	TransactionID string
	Card          string
	// EventTime is the timestamp carried on the authorization itself. Every
	// window and every gap is measured against it, never against the clock.
	EventTime time.Time
	AmountUSD float64
}

// A Verdict is approve, decline or challenge.
type Verdict = decisioning.Verdict

const (
	Approve   = decisioning.Approve
	Decline   = decisioning.Decline
	Challenge = decisioning.Challenge
)

// A Decision explains itself: everything that went into the verdict is here, so
// a reviewer can recompute by hand why this authorization got this answer.
type Decision struct {
	Authorization Authorization
	// Signals is keyed by the names in the model's signal set. A missing gap --
	// a card's first payment -- is NaN.
	Signals map[string]float64
	// RiskScore is calibrated: 0.3 means fraud roughly three times in ten.
	RiskScore     float64
	Verdict       Verdict
	ExpectedCosts map[Verdict]float64
	// Degraded says the risk score is the configured base fraud rate rather
	// than the model's, because something inside the system failed. The verdict
	// still comes from the same expected-cost formula; there is no second policy.
	Degraded       bool
	DegradedReason string
}

// SignalSet is the signals the model reads, in the order it reads them.
func SignalSet() []string { return append([]string(nil), signals.Set...) }

// Config points at the artifacts and the data. Everything here is a path
// because everything here is chosen outside the code.
type Config struct {
	ModelPath       string
	CalibrationPath string
	CostsPath       string

	// PaymentHistory is the payment files the signal store is populated from.
	// Populating the store is deliberately separable from scoring: a real
	// background process replaces this and touches nothing else.
	PaymentHistory []string

	// HistoryPath is where scored authorizations are persisted. Empty means
	// they are not; the decision is unaffected either way.
	HistoryPath string
	// HistoryBuffer is how many scored authorizations may be waiting to be
	// persisted before further ones are dropped. Persistence must never sit
	// between an authorization and its decision, so this queue is never waited on.
	HistoryBuffer int

	Logger *slog.Logger
}

// DefaultConfig is the checked-in artifacts and data, as laid out in this repo.
func DefaultConfig() Config {
	return Config{
		ModelPath:       "artifacts/model.txt",
		CalibrationPath: "artifacts/calibration.json",
		CostsPath:       "config/costs.json",
		// The small checked-in slice, so a fresh clone can score something.
		// Point this at data/fraudTrain.csv and data/fraudTest.csv for the lot.
		PaymentHistory: []string{"data/fixture_payments.csv"},
		HistoryBuffer:  1024,
	}
}

// A Scorer holds everything loaded once at startup.
type Scorer struct {
	risk   *risk.Scorer
	costs  decisioning.Costs
	store  *signals.Store
	logger *slog.Logger

	persist   chan history.Record
	persisted sync.WaitGroup
	writer    *history.Writer
	closing   sync.Once
}

// New loads the artifacts and the card history. A missing or malformed artifact
// fails here, so a broken deployment cannot present as quietly bad decisions.
func New(config Config) (*Scorer, error) {
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}

	riskScorer, err := risk.Load(config.ModelPath, config.CalibrationPath)
	if err != nil {
		return nil, err
	}
	if got, want := riskScorer.FeatureCount(), len(signals.Set); got != want {
		return nil, fmt.Errorf("model %s reads %d signals, this build derives %d (%v)",
			config.ModelPath, got, want, signals.Set)
	}

	costs, err := decisioning.LoadCosts(config.CostsPath)
	if err != nil {
		return nil, err
	}

	store := signals.NewStore()
	if err := signals.LoadCSV(store, config.PaymentHistory...); err != nil {
		return nil, err
	}

	scorer := &Scorer{risk: riskScorer, costs: costs, store: store, logger: logger}
	if config.HistoryPath != "" {
		if scorer.writer, err = history.Open(config.HistoryPath); err != nil {
			return nil, err
		}
		scorer.startPersisting(max(config.HistoryBuffer, 1))
	}
	return scorer, nil
}

// Score answers one authorization. It always answers: a failure inside the
// system degrades the risk score to the configured base fraud rate and runs the
// same arithmetic, rather than leaving the authorization unanswered.
func (s *Scorer) Score(ctx context.Context, auth Authorization) Decision {
	derived, riskScore, degradedReason := s.assess(ctx, auth)
	verdict, expected := s.costs.Cheapest(riskScore, auth.AmountUSD)

	decision := Decision{
		Authorization:  auth,
		Signals:        derived,
		RiskScore:      riskScore,
		Verdict:        verdict,
		ExpectedCosts:  expected,
		Degraded:       degradedReason != "",
		DegradedReason: degradedReason,
	}
	s.persistBehindResponse(decision)
	return decision
}

// assess derives the signals and scores them, or explains why it could not.
func (s *Scorer) assess(ctx context.Context, auth Authorization) (map[string]float64, float64, string) {
	fallback := func(reason string) (map[string]float64, float64, string) {
		return nil, s.costs.BaseFraudRate, reason
	}

	if err := ctx.Err(); err != nil {
		return fallback(fmt.Sprintf("no time left to score: %v", err))
	}

	derived := signals.Derive(
		s.store.History(auth.Card),
		signals.Payment{TransactionID: auth.TransactionID, EventTime: auth.EventTime.Unix()},
		auth.AmountUSD,
	)

	riskScore, err := s.risk.Score(signals.Vector(derived))
	if err != nil {
		s.logger.Error("scoring failed, degrading to the base fraud rate",
			"transaction_id", auth.TransactionID, "err", err)
		_, score, reason := fallback(err.Error())
		return derived, score, reason
	}
	if math.IsNaN(riskScore) || riskScore < 0 || riskScore > 1 {
		reason := fmt.Sprintf("risk score %v is not a probability", riskScore)
		s.logger.Error("scoring failed, degrading to the base fraud rate",
			"transaction_id", auth.TransactionID, "err", reason)
		return derived, s.costs.BaseFraudRate, reason
	}
	return derived, riskScore, ""
}

func (s *Scorer) startPersisting(buffer int) {
	s.persist = make(chan history.Record, buffer)
	s.persisted.Add(1)
	go func() {
		defer s.persisted.Done()
		for record := range s.persist {
			if err := s.writer.Append(record); err != nil {
				// The history path must not be able to degrade the decision
				// path, and the decision has already been returned.
				s.logger.Error("persisting to durable history failed",
					"transaction_id", record.TransactionID, "err", err)
			}
		}
	}()
}

// persistBehindResponse hands the record off without waiting for it. If the
// queue is full the record is dropped, because the alternative is a durable
// write sitting between an authorization and its decision.
func (s *Scorer) persistBehindResponse(decision Decision) {
	if s.persist == nil {
		return
	}
	select {
	case s.persist <- recordOf(decision):
	default:
		s.logger.Warn("durable history is behind; dropping a record",
			"transaction_id", decision.Authorization.TransactionID)
	}
}

func recordOf(decision Decision) history.Record {
	expected := make(map[string]float64, len(decision.ExpectedCosts))
	for verdict, cost := range decision.ExpectedCosts {
		expected[string(verdict)] = cost
	}
	// Copied, because the record is written after the caller has the decision
	// and is free to do what it likes with the maps in it.
	derived := make(map[string]float64, len(decision.Signals))
	for name, value := range decision.Signals {
		derived[name] = value
	}

	return history.Record{
		TransactionID: decision.Authorization.TransactionID,
		Card:          decision.Authorization.Card,
		EventTime:     decision.Authorization.EventTime.Unix(),
		AmountUSD:     decision.Authorization.AmountUSD,
		Signals:       derived,
		RiskScore:     decision.RiskScore,
		Verdict:       string(decision.Verdict),
		ExpectedCosts: expected,
		Degraded:      decision.Degraded,
		ScoredAt:      time.Now().Unix(),
	}
}

// Close drains whatever is still queued for durable history and closes the file.
func (s *Scorer) Close() error {
	var err error
	s.closing.Do(func() {
		if s.persist != nil {
			close(s.persist)
			s.persisted.Wait()
		}
		if s.writer != nil {
			err = s.writer.Close()
		}
	})
	return err
}
