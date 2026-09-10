// Command pipeline consumes authorizations from Kafka, scores them with the
// four heuristics in internal/heuristics, and routes each one to the approved
// or the review topic.
//
//	go run ./cmd/pipeline -demo          # no broker needed; in-process fake
//	go run ./cmd/pipeline                # against localhost:9092
//	go run ./cmd/pipeline -brokers host:9092 -in authorizations
//
// This is the explored alternative recorded in ADR-0007. The decision path this
// repo actually argues for is `cmd/score`, which is unaffected by anything here.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/internal/heuristics"
	"github.com/DevikaVishnu/fraud-scoring/internal/pipeline"
	"github.com/DevikaVishnu/fraud-scoring/internal/signals"
	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

func main() {
	var (
		demo       = flag.Bool("demo", false, "run against an in-process fake broker, so no Kafka is needed")
		brokers    = flag.String("brokers", "localhost:9092", "comma-separated Kafka brokers")
		group      = flag.String("group", "fraud-scoring", "consumer group")
		inTopic    = flag.String("in", "authorizations", "topic to consume authorizations from")
		approved   = flag.String("approved", "approved", "topic for authorizations that pass")
		review     = flag.String("review", "review", "topic for authorizations a human should see")
		thresholds = flag.String("thresholds", "config/thresholds.json", "tuned rule thresholds")
		payments   = flag.String("payments", "data/fixture_payments.csv", "comma-separated payment files to populate the signal store from")
		perCard    = flag.Int("demo-per-card", 4, "authorizations to synthesize per card in demo mode")
		seedOnly   = flag.Bool("seed", false, "produce synthetic authorizations to the input topic and exit")
		idle       = flag.Duration("idle", 15*time.Second, "stop after this long with no message; 0 to run until stopped. Must outlast the initial consumer-group join, which takes a few seconds")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Ctrl-C stops the loop between messages, so nothing is cut off between
	// being produced and being committed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *seedOnly {
		if err := stream.EnsureTopics(strings.Split(*brokers, ","), *inTopic); err != nil {
			log.Fatal(err)
		}
		if err := seed(ctx, strings.Split(*brokers, ","), *inTopic,
			strings.Split(*payments, ",")[0], *perCard); err != nil {
			log.Fatal(err)
		}
		return
	}

	tuned, err := heuristics.LoadThresholds(*thresholds)
	if err != nil {
		log.Fatal(err)
	}
	store := signals.NewStore()
	if err := signals.LoadCSV(store, strings.Split(*payments, ",")...); err != nil {
		log.Fatal(err)
	}

	source, sink, err := open(*demo, *payments, *perCard, *brokers, *group, *inTopic, *approved, *review)
	if err != nil {
		log.Fatal(err)
	}
	defer source.Close()
	defer sink.Close()
	source = idleStopping{Source: source, after: *idle}

	topics := pipeline.Topics{Approved: *approved, Review: *review}
	runner := pipeline.New(source, sink, store, tuned, topics, logger)

	fmt.Fprintf(os.Stderr, "consuming %s -> %s / %s (%d cards of history)\n",
		*inTopic, *approved, *review, store.Cards())
	if err := runner.Run(ctx); err != nil {
		log.Fatal(err)
	}

	fmt.Fprintf(os.Stderr, "\nrouted %d approved, %d review, %d unscorable\n",
		runner.Approved, runner.Review, runner.Unscorable)
	if fake, ok := sink.(*stream.Fake); ok {
		reportDemo(os.Stdout, fake, topics)
	}
}

func open(demo bool, payments string, perCard int, brokers, group, inTopic, approved, review string) (stream.Source, stream.Sink, error) {
	if demo {
		authorizations, err := demoAuthorizations(strings.Split(payments, ",")[0], perCard)
		if err != nil {
			return nil, nil, err
		}
		fake := stream.NewFake(authorizations...)
		return fake, fake, nil
	}
	config := stream.KafkaConfig{
		Brokers: strings.Split(brokers, ","),
		Group:   group,
		InTopic: inTopic,
	}
	// Both destinations, not just the source: a topic that appears only when
	// the first message is routed to it is a topic that is missing exactly
	// when someone goes looking for an empty review queue.
	if err := stream.EnsureTopics(config.Brokers, inTopic, approved, review); err != nil {
		return nil, nil, err
	}
	return stream.OpenKafkaSource(config), stream.OpenKafkaSink(config.Brokers), nil
}

// reportDemo prints what landed on each topic, so a demo run shows the routing
// rather than just asserting it happened.
func reportDemo(out *os.File, fake *stream.Fake, topics pipeline.Topics) {
	for _, topic := range []string{topics.Approved, topics.Review} {
		messages := fake.Written(topic)
		fmt.Fprintf(out, "\n=== %s (%d) ===\n", topic, len(messages))
		for _, raw := range messages {
			var routed pipeline.Routed
			if err := json.Unmarshal(raw, &routed); err != nil {
				fmt.Fprintf(out, "  <unreadable: %v>\n", err)
				continue
			}
			fmt.Fprintf(out, "  %-28s $%9.2f  %3.0f points", routed.TransactionID, routed.AmountUSD, routed.Points)
			if routed.Unscorable != "" {
				fmt.Fprintf(out, "  UNSCORABLE: %s", routed.Unscorable)
			}
			for _, fired := range routed.Fired {
				fmt.Fprintf(out, "\n      fired %-18s %s", fired.Rule, fired.Why)
			}
			fmt.Fprintln(out)
		}
	}
}
