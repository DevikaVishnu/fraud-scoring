package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/DevikaVishnu/fraud-scoring/internal/heuristics"
	"github.com/DevikaVishnu/fraud-scoring/internal/signals"
	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

var tuned = heuristics.Thresholds{
	RapidSuccessionSeconds: 5,
	HourlyVelocityAt:       5,
	DailyVelocityAt:        20,
	LargeAmountUSD:         1000,
	ReviewAt:               40,
}

var topics = Topics{Approved: "approved", Review: "review"}

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// history is one card with two payments, small enough to reason about.
func history(t *testing.T) *signals.Store {
	t.Helper()
	store := signals.NewStore()
	store.Record("card-1", signals.Payment{TransactionID: "a", EventTime: 1000})
	store.Record("card-1", signals.Payment{TransactionID: "b", EventTime: 2000})
	store.Sort()
	return store
}

func run(t *testing.T, store *signals.Store, authorizations ...stream.Authorization) (*Pipeline, *stream.Fake) {
	t.Helper()
	fake := stream.NewFake(authorizations...)
	runner := New(fake, fake, store, tuned, topics, quiet())
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return runner, fake
}

func decode(t *testing.T, raw []byte) Routed {
	t.Helper()
	var routed Routed
	if err := json.Unmarshal(raw, &routed); err != nil {
		t.Fatalf("decoding routed message: %v", err)
	}
	return routed
}

func TestAnOrdinaryAuthorizationIsRoutedToApproved(t *testing.T) {
	_, fake := run(t, history(t), stream.Authorization{
		TransactionID: "c", Card: "card-1", EventTime: 20000, AmountUSD: 50,
	})

	if got := len(fake.Written(topics.Review)); got != 0 {
		t.Errorf("review received %d messages, want 0", got)
	}
	messages := fake.Written(topics.Approved)
	if len(messages) != 1 {
		t.Fatalf("approved received %d messages, want 1", len(messages))
	}
	routed := decode(t, messages[0])
	if routed.Route != string(heuristics.Approved) || routed.Points != 0 {
		t.Errorf("routed = %+v, want approved with 0 points", routed)
	}
	// The routed message carries the derived signals, so a consumer never has
	// to re-derive them.
	if routed.Signals["gap_seconds"] != 18000 {
		t.Errorf("gap_seconds = %v, want 18000", routed.Signals["gap_seconds"])
	}
}

func TestAnAuthorizationThatFiresARuleIsRoutedToReview(t *testing.T) {
	// Two seconds after the card's last payment: rapid succession, 40 points.
	_, fake := run(t, history(t), stream.Authorization{
		TransactionID: "c", Card: "card-1", EventTime: 2002, AmountUSD: 50,
	})

	if got := len(fake.Written(topics.Approved)); got != 0 {
		t.Errorf("approved received %d messages, want 0", got)
	}
	messages := fake.Written(topics.Review)
	if len(messages) != 1 {
		t.Fatalf("review received %d messages, want 1", len(messages))
	}
	routed := decode(t, messages[0])
	if len(routed.Fired) != 1 || routed.Fired[0].Rule != "rapid_succession" {
		t.Fatalf("fired = %+v, want rapid_succession", routed.Fired)
	}
	// The reason travels with the message, so a review queue can show it.
	if routed.Fired[0].Why == "" {
		t.Error("the rule that fired carries no reason")
	}
}

// The routing is what makes these heuristics defensible at all (ADR-0007), so
// both destinations must actually be reachable in one run.
func TestBothTopicsAreReachable(t *testing.T) {
	runner, fake := run(t, history(t),
		stream.Authorization{TransactionID: "c", Card: "card-1", EventTime: 20000, AmountUSD: 50},
		stream.Authorization{TransactionID: "d", Card: "card-1", EventTime: 2002, AmountUSD: 50},
	)
	if len(fake.Written(topics.Approved)) != 1 || len(fake.Written(topics.Review)) != 1 {
		t.Fatalf("approved %d, review %d; want one each",
			len(fake.Written(topics.Approved)), len(fake.Written(topics.Review)))
	}
	if runner.Approved != 1 || runner.Review != 1 {
		t.Errorf("counted approved %d review %d, want one each", runner.Approved, runner.Review)
	}
}

// A signal never contains information from its own outcome, and a rule reads
// only signals, so this holds through the pipeline too.
func TestSignalsReadOnlyPaymentsStrictlyBeforeTheAuthorization(t *testing.T) {
	store := history(t)
	// An authorization at the same event time as an existing payment, with a
	// transaction id sorting after it: the existing payment is a predecessor.
	_, fake := run(t, store, stream.Authorization{
		TransactionID: "z", Card: "card-1", EventTime: 2000, AmountUSD: 50,
	})
	routed := decode(t, append(fake.Written(topics.Approved), fake.Written(topics.Review)...)[0])
	if routed.Signals["gap_seconds"] != 0 {
		t.Errorf("gap_seconds = %v, want 0 (the payment at the same second precedes it)",
			routed.Signals["gap_seconds"])
	}
	if routed.Signals["velocity_1h"] != 2 {
		t.Errorf("velocity_1h = %v, want 2 (both predecessors, not the authorization itself)",
			routed.Signals["velocity_1h"])
	}
}

// An unknown card is the same case as a card's first payment: no history, no
// gap, and nothing fires.
func TestAnUnknownCardIsScorable(t *testing.T) {
	runner, fake := run(t, history(t), stream.Authorization{
		TransactionID: "c", Card: "card-unseen", EventTime: 20000, AmountUSD: 50,
	})
	if runner.Approved != 1 {
		t.Fatalf("approved %d, want 1", runner.Approved)
	}
	raw := fake.Written(topics.Approved)[0]
	if !json.Valid(raw) {
		t.Fatalf("routed message is not valid json: %s", raw)
	}
	routed := decode(t, raw)
	if routed.Points != 0 {
		t.Errorf("points = %v, want 0", routed.Points)
	}
	// The gap is undefined, not zero. NaN has no JSON representation, so it is
	// absent from the wire message -- and a consumer must not read the absence
	// as a very short gap.
	if _, present := routed.Signals["gap_seconds"]; present {
		t.Errorf("gap_seconds = %v, want it absent for a card with no history",
			routed.Signals["gap_seconds"])
	}
	if routed.Signals["velocity_24h"] != 0 {
		t.Errorf("velocity_24h = %v, want 0", routed.Signals["velocity_24h"])
	}
}

// A message that cannot be scored goes to review, not approved: the failure is
// in this system, and a cardholder should not be approved by default because
// of it.
func TestAnUnscorableMessageIsRoutedToReview(t *testing.T) {
	for _, test := range []struct{ name, payload string }{
		{"not json", `not json at all`},
		{"no transaction id", `{"card":"card-1","event_time":20000,"amount_usd":50}`},
		{"no card", `{"transaction_id":"c","event_time":20000,"amount_usd":50}`},
		{"no event time", `{"transaction_id":"c","card":"card-1","amount_usd":50}`},
		{"negative amount", `{"transaction_id":"c","card":"card-1","event_time":20000,"amount_usd":-5}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := stream.NewFakeRaw([]byte(test.payload))
			runner := New(fake, fake, history(t), tuned, topics, quiet())
			if err := runner.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := len(fake.Written(topics.Approved)); got != 0 {
				t.Errorf("approved received %d messages, want 0", got)
			}
			messages := fake.Written(topics.Review)
			if len(messages) != 1 {
				t.Fatalf("review received %d messages, want 1", len(messages))
			}
			if routed := decode(t, messages[0]); routed.Unscorable == "" {
				t.Error("routed to review with no reason recorded")
			}
			if runner.Unscorable != 1 {
				t.Errorf("counted %d unscorable, want 1", runner.Unscorable)
			}
		})
	}
}

// The ordering contract. A message must be produced before its offset is
// committed: a crash in between replays it, which is a duplicate in a queue,
// while the other order loses an authorization silently.
func TestAMessageIsProducedBeforeItIsCommitted(t *testing.T) {
	fake := stream.NewFake(stream.Authorization{
		TransactionID: "c", Card: "card-1", EventTime: 20000, AmountUSD: 50,
	})
	runner := New(fake, &orderRecording{Sink: fake, t: t}, history(t), tuned, topics, quiet())
	if err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fake.Committed(); len(got) != 1 {
		t.Fatalf("committed %v, want exactly one offset", got)
	}
}

// orderRecording asserts, at the moment of the write, that nothing has been
// committed yet.
type orderRecording struct {
	stream.Sink
	fake *stream.Fake
	t    *testing.T
}

func (o *orderRecording) Write(ctx context.Context, topic, key string, value []byte) error {
	if fake, ok := o.Sink.(*stream.Fake); ok {
		if committed := fake.Committed(); len(committed) != 0 {
			o.t.Errorf("offsets %v were committed before the message was produced", committed)
		}
	}
	return o.Sink.Write(ctx, topic, key, value)
}

// A failure to produce must not commit, so the message is redelivered rather
// than silently dropped.
func TestAFailureToProduceDoesNotCommit(t *testing.T) {
	fake := stream.NewFake(stream.Authorization{
		TransactionID: "c", Card: "card-1", EventTime: 20000, AmountUSD: 50,
	})
	runner := New(fake, failingSink{}, history(t), tuned, topics, quiet())

	err := runner.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil, want the produce failure surfaced")
	}
	if got := fake.Committed(); len(got) != 0 {
		t.Errorf("committed %v after a failed produce, want nothing committed", got)
	}
	if runner.Approved != 0 || runner.Review != 0 {
		t.Errorf("counted a routed message that was never produced")
	}
}

type failingSink struct{}

func (failingSink) Write(context.Context, string, string, []byte) error {
	return errors.New("broker unavailable")
}
func (failingSink) Close() error { return nil }

// A cancelled context stops the loop between messages rather than mid-message,
// so nothing is cut off between being produced and being committed.
func TestACancelledContextStopsCleanly(t *testing.T) {
	fake := stream.NewFake(stream.Authorization{
		TransactionID: "c", Card: "card-1", EventTime: 20000, AmountUSD: 50,
	})
	runner := New(fake, fake, history(t), tuned, topics, quiet())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("Run on a cancelled context: %v, want a clean stop", err)
	}
	if got := len(fake.Written(topics.Approved)) + len(fake.Written(topics.Review)); got != 0 {
		t.Errorf("routed %d messages on a cancelled context, want 0", got)
	}
}
