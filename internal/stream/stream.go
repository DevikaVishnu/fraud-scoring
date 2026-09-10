// Package stream carries authorizations in and routed decisions out.
//
// The pipeline talks to these interfaces and never to Kafka directly, so the
// same loop runs against a real broker and against the in-process fake below.
// That is not only a testing convenience: it is what lets `make pipeline-demo`
// work on a clone with nothing installed, the way `make score` already does.
//
// Note the ordering contract this package asks of its caller, because it is the
// part that is easy to get wrong. Delivery is at-least-once: a message must be
// produced to its destination topic *before* its source offset is committed. A
// crash in between replays the authorization, which routes it twice; a crash
// the other way around loses it silently. Routing something twice is a
// duplicate in a review queue, and losing it is an unscored payment.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrClosed is returned by Read when the source is exhausted and will produce
// no further messages. The fake returns it at the end of its input; a real
// broker generally does not, because a topic has no end.
var ErrClosed = errors.New("stream: closed")

// An Authorization is the message shape on the input topic. It is the wire
// contract, kept separate from the domain type in `scoring` on purpose: a
// producer on another team owns this shape, and letting it drift into the
// internals is how a schema change becomes a code change everywhere.
type Authorization struct {
	TransactionID string  `json:"transaction_id"`
	Card          string  `json:"card"`
	EventTime     int64   `json:"event_time"` // unix seconds, per CONTEXT.md
	AmountUSD     float64 `json:"amount_usd"`
}

// Validate rejects a message that cannot be scored. A malformed authorization
// is a poison pill: it will fail identically on every replay, so the pipeline
// must be able to tell it apart from a transient failure and set it aside.
func (a Authorization) Validate() error {
	switch {
	case a.TransactionID == "":
		return errors.New("no transaction id")
	case a.Card == "":
		return errors.New("no card")
	case a.EventTime <= 0:
		return fmt.Errorf("event time %d is not a unix timestamp", a.EventTime)
	case a.AmountUSD < 0:
		return fmt.Errorf("amount %v is negative", a.AmountUSD)
	}
	return nil
}

// A Message is one authorization and whatever the source needs to commit it.
type Message struct {
	Authorization Authorization
	// Raw is the bytes as they arrived, kept so a message that fails to parse
	// can still be forwarded somewhere a human can look at it.
	Raw []byte
	// offset is opaque to the pipeline; each Source interprets its own.
	offset any
}

// A Source hands out authorizations and records which ones are done with.
type Source interface {
	// Read blocks until the next message, the context is done, or the source is
	// exhausted (ErrClosed).
	Read(ctx context.Context) (Message, error)
	// Commit records that the message has been fully handled. Call it only
	// after the routed result has been produced.
	Commit(ctx context.Context, msg Message) error
	Close() error
}

// A Sink writes a routed authorization to a named topic. The key is the card,
// so every payment on one card lands on one partition and stays in order.
type Sink interface {
	Write(ctx context.Context, topic, key string, value []byte) error
	Close() error
}

// Encode marshals a routed result for a destination topic.
func Encode(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding message: %w", err)
	}
	return raw, nil
}

// --- the in-process fake ---------------------------------------------------

// A Fake is a Source and a Sink backed by channels, for tests and for a demo
// run with no broker. It behaves like the real thing in the ways the pipeline
// can observe: reads block, commits are recorded, and writes are ordered per
// topic.
type Fake struct {
	incoming chan Message

	mu        sync.Mutex
	written   map[string][][]byte
	committed []string
	closed    bool
}

// NewFake builds a fake pre-loaded with authorizations to hand out.
func NewFake(authorizations ...Authorization) *Fake {
	incoming := make(chan Message, len(authorizations))
	for _, auth := range authorizations {
		raw, _ := json.Marshal(auth)
		incoming <- Message{Authorization: auth, Raw: raw, offset: auth.TransactionID}
	}
	close(incoming)
	return &Fake{incoming: incoming, written: map[string][][]byte{}}
}

// NewFakeRaw builds a fake over arbitrary bytes, so a malformed message can be
// put through the pipeline the way a real producer could send one.
func NewFakeRaw(payloads ...[]byte) *Fake {
	incoming := make(chan Message, len(payloads))
	for i, raw := range payloads {
		msg := Message{Raw: raw, offset: i}
		_ = json.Unmarshal(raw, &msg.Authorization) // may fail; the pipeline decides
		incoming <- msg
	}
	close(incoming)
	return &Fake{incoming: incoming, written: map[string][][]byte{}}
}

func (f *Fake) Read(ctx context.Context) (Message, error) {
	select {
	case <-ctx.Done():
		return Message{}, ctx.Err()
	case msg, ok := <-f.incoming:
		if !ok {
			return Message{}, ErrClosed
		}
		return msg, nil
	}
}

func (f *Fake) Commit(_ context.Context, msg Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.committed = append(f.committed, fmt.Sprint(msg.offset))
	return nil
}

func (f *Fake) Write(_ context.Context, topic, _ string, value []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("stream: write to a closed fake")
	}
	f.written[topic] = append(f.written[topic], append([]byte(nil), value...))
	return nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Written is everything sent to a topic, in order.
func (f *Fake) Written(topic string) [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.written[topic]...)
}

// Committed is the offsets committed, in order, so a test can assert that a
// message was not committed before its result was produced.
func (f *Fake) Committed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.committed...)
}

// Timeout is the read timeout a real source uses when a topic goes quiet.
const Timeout = 10 * time.Second
