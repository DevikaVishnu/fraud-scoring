// Package pipeline is the loop: read an authorization, derive its signals,
// apply the heuristics, route it, then commit.
//
// The order of those last two steps is the whole correctness argument and is
// stated in stream's doc comment: produce, then commit. Everything else here is
// bookkeeping around that.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/internal/heuristics"
	"github.com/DevikaVishnu/fraud-scoring/internal/signals"
	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

// A Routed message is what lands on the destination topic. It explains itself
// the way `history.Record` does: an operator opening the review queue gets the
// signals, the rules that fired and why, without re-deriving anything.
type Routed struct {
	TransactionID string             `json:"transaction_id"`
	Card          string             `json:"card"`
	EventTime     int64              `json:"event_time"`
	AmountUSD     float64            `json:"amount_usd"`
	Signals       map[string]float64 `json:"signals,omitempty"`
	Points        float64            `json:"points"`
	Route         string             `json:"route"`
	Fired         []heuristics.Fired `json:"fired,omitempty"`
	// Unscorable carries why a message could not be assessed. It is set only on
	// messages routed to review without an assessment.
	Unscorable string `json:"unscorable,omitempty"`
	ScoredAt   int64  `json:"scored_at"`
}

// Topics names the two destinations.
type Topics struct {
	Approved string
	Review   string
}

// A Pipeline holds everything loaded once, the way scoring.Scorer does.
type Pipeline struct {
	source     stream.Source
	sink       stream.Sink
	store      *signals.Store
	thresholds heuristics.Thresholds
	topics     Topics
	logger     *slog.Logger

	// Stats are counted so a run can report what it did.
	Approved, Review, Unscorable int
}

func New(source stream.Source, sink stream.Sink, store *signals.Store,
	thresholds heuristics.Thresholds, topics Topics, logger *slog.Logger) *Pipeline {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{
		source: source, sink: sink, store: store,
		thresholds: thresholds, topics: topics, logger: logger,
	}
}

// Run consumes until the source is exhausted or the context is done.
func (p *Pipeline) Run(ctx context.Context) error {
	for {
		// Checked before the read, not only inside it: a source whose Read
		// selects on both the context and a ready message may legally pick
		// either, and a cancelled pipeline must not route one more.
		if ctx.Err() != nil {
			return nil
		}
		msg, err := p.source.Read(ctx)
		switch {
		case errors.Is(err, stream.ErrClosed):
			return nil
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return nil
		case err != nil:
			return err
		}
		if err := p.handle(ctx, msg); err != nil {
			return err
		}
	}
}

// handle routes one message and commits it. A failure to produce returns
// without committing, so the message is redelivered rather than lost.
func (p *Pipeline) handle(ctx context.Context, msg stream.Message) error {
	routed := p.assess(msg.Authorization)

	topic := p.topics.Approved
	if routed.Route == string(heuristics.Review) {
		topic = p.topics.Review
	}
	value, err := stream.Encode(routed)
	if err != nil {
		return err
	}
	// Produce first. The commit below is what makes this message done, and
	// doing it in the other order turns a crash into a silently unscored
	// payment (see stream's doc comment).
	if err := p.sink.Write(ctx, topic, routed.Card, value); err != nil {
		return fmt.Errorf("routing %s to %s: %w", routed.TransactionID, topic, err)
	}
	if err := p.source.Commit(ctx, msg); err != nil {
		// The result is already on the destination topic. Failing to commit
		// means it will be produced again after a restart, which the routing
		// design tolerates: a duplicate in a review queue is a nuisance, not a
		// wrong decision.
		return fmt.Errorf("committing %s after routing it: %w", routed.TransactionID, err)
	}

	switch {
	case routed.Unscorable != "":
		p.Unscorable++
	case routed.Route == string(heuristics.Review):
		p.Review++
	default:
		p.Approved++
	}
	return nil
}

// assess derives the signals and applies the rules, or explains why it could
// not. An authorization that cannot be scored is routed to review rather than
// approved: the failure is in this system, and the cardholder should not be
// approved by default because of it.
func (p *Pipeline) assess(auth stream.Authorization) Routed {
	routed := Routed{
		TransactionID: auth.TransactionID,
		Card:          auth.Card,
		EventTime:     auth.EventTime,
		AmountUSD:     auth.AmountUSD,
		ScoredAt:      time.Now().Unix(),
	}

	if err := auth.Validate(); err != nil {
		p.logger.Warn("unscorable authorization routed to review",
			"transaction_id", auth.TransactionID, "err", err)
		routed.Route = string(heuristics.Review)
		routed.Unscorable = err.Error()
		return routed
	}

	// The same derivation the scoring path uses, against the same store. The
	// heuristics read derived signals, not raw fields, so a rule cannot
	// accidentally disagree with the model about what "the gap" means.
	derived := signals.Derive(
		p.store.History(auth.Card),
		signals.Payment{TransactionID: auth.TransactionID, EventTime: auth.EventTime},
		auth.AmountUSD,
	)
	assessment := p.thresholds.Assess(heuristics.Signals{
		AmountUSD:   derived["amount"],
		GapSeconds:  derived["gap_seconds"],
		Velocity1h:  derived["velocity_1h"],
		Velocity24h: derived["velocity_24h"],
	})

	routed.Signals = forTheWire(derived)
	routed.Points = assessment.Points
	routed.Route = string(assessment.Route)
	routed.Fired = assessment.Fired
	return routed
}

// forTheWire drops signals that are undefined for this authorization.
//
// A card's first payment has no gap, which internal/signals states as NaN --
// the right in-process answer, and one JSON cannot encode at all. Dropping the
// key says the same thing on the wire: the signal is absent because it is
// undefined, not zero. A consumer that reads a missing gap as a short one is
// making the mistake this repo has already made once (ADR-0006).
func forTheWire(derived map[string]float64) map[string]float64 {
	wire := make(map[string]float64, len(derived))
	for name, value := range derived {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		wire[name] = value
	}
	return wire
}
