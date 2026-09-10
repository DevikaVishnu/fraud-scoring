package main

import (
	"context"
	"fmt"
	"os"

	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

// seed fills the input topic with the same synthetic authorizations the demo
// runs against, so the real Kafka path can be exercised without a producer on
// the other end of it.
func seed(ctx context.Context, brokers []string, topic, payments string, perCard int) error {
	authorizations, err := demoAuthorizations(payments, perCard)
	if err != nil {
		return err
	}
	sink := stream.OpenKafkaSink(brokers)
	defer sink.Close()

	for _, auth := range authorizations {
		value, err := stream.Encode(auth)
		if err != nil {
			return err
		}
		if err := sink.Write(ctx, topic, auth.Card, value); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "seeded %d authorizations to %s\n", len(authorizations), topic)
	return nil
}
