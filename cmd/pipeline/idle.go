package main

import (
	"context"
	"errors"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

// idleStopping ends the run when a topic has been quiet for a while.
//
// A real consumer runs until it is stopped, because a topic has no end -- which
// is correct and also makes a demo run impossible to finish. This wraps the
// source with a per-read deadline and reports a quiet topic as exhaustion, so
// `make pipeline` terminates the way `make pipeline-demo` does. Zero disables
// it, which is the right setting for anything long-running.
type idleStopping struct {
	stream.Source
	after time.Duration
}

func (i idleStopping) Read(ctx context.Context) (stream.Message, error) {
	if i.after <= 0 {
		return i.Source.Read(ctx)
	}
	deadlined, cancel := context.WithTimeout(ctx, i.after)
	defer cancel()

	msg, err := i.Source.Read(deadlined)
	// Only our own deadline means "quiet"; a cancellation from the caller is
	// the caller's business and is passed through untouched.
	if err != nil && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return stream.Message{}, stream.ErrClosed
	}
	return msg, err
}
