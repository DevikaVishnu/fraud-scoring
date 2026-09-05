// Command score puts one authorization through the whole path and prints the
// decision, so the system can be seen working within a minute of cloning.
//
//	go run ./cmd/score
//	go run ./cmd/score -card 372520049757633 -amount 1200 -at 2013-12-08T00:12:46Z
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"strings"
	"time"

	"github.com/DevikaVishnu/fraud-scoring/scoring"
)

// The defaults score a real card from the checked-in history slice, five
// minutes after its last payment there, so the velocity counters have something
// to say. See testdata/train_serve_fixture.json for where the card comes from.
const (
	defaultCard      = "372520049757633"
	defaultEventTime = "2013-12-08T00:12:46Z"
	defaultAmount    = 250.00
)

func main() {
	config := scoring.DefaultConfig()
	auth := scoring.Authorization{TransactionID: "demo-authorization"}

	flag.StringVar(&auth.TransactionID, "id", auth.TransactionID, "transaction id")
	flag.StringVar(&auth.Card, "card", defaultCard, "card the payment is on")
	flag.Float64Var(&auth.AmountUSD, "amount", defaultAmount, "payment amount in dollars")
	at := flag.String("at", defaultEventTime, "event time, RFC 3339")
	payments := flag.String("payments", strings.Join(config.PaymentHistory, ","),
		"comma-separated payment files to populate the signal store from")
	flag.StringVar(&config.ModelPath, "model", config.ModelPath, "LightGBM model file")
	flag.StringVar(&config.CalibrationPath, "calibration", config.CalibrationPath, "calibration table")
	flag.StringVar(&config.CostsPath, "costs", config.CostsPath, "cost inputs")
	flag.StringVar(&config.HistoryPath, "history", "", "Parquet file to persist the scored authorization to")
	flag.Parse()

	eventTime, err := time.Parse(time.RFC3339, *at)
	if err != nil {
		log.Fatalf("event time %q: %v", *at, err)
	}
	auth.EventTime = eventTime
	config.PaymentHistory = strings.Split(*payments, ",")

	scorer, err := scoring.New(config)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := scorer.Close(); err != nil {
			log.Print(err)
		}
	}()

	report(os.Stdout, scorer.Score(context.Background(), auth))
}

// report prints every input the verdict was computed from, so a reader can
// recompute it by hand.
func report(out *os.File, decision scoring.Decision) {
	auth := decision.Authorization
	fmt.Fprintf(out, "authorization %s on card %s: $%.2f at %s\n\n",
		auth.TransactionID, auth.Card, auth.AmountUSD,
		auth.EventTime.UTC().Format(time.RFC3339))

	fmt.Fprintln(out, "signals")
	for _, name := range scoring.SignalSet() {
		value := decision.Signals[name]
		if math.IsNaN(value) {
			fmt.Fprintf(out, "  %-14s %14s   (no previous payment on this card)\n", name, "none")
			continue
		}
		fmt.Fprintf(out, "  %-14s %14.2f\n", name, value)
	}

	fmt.Fprintf(out, "\nrisk score %.6f  (calibrated: fraud roughly %.1f times in 10,000)\n",
		decision.RiskScore, decision.RiskScore*10000)
	if decision.Degraded {
		fmt.Fprintf(out, "  DEGRADED to the base fraud rate: %s\n", decision.DegradedReason)
	}

	fmt.Fprintln(out, "\nexpected cost of each decision, in dollars")
	for _, verdict := range []scoring.Verdict{scoring.Approve, scoring.Decline, scoring.Challenge} {
		fmt.Fprintf(out, "  %-14s %14.4f\n", verdict, decision.ExpectedCosts[verdict])
	}

	fmt.Fprintf(out, "\ndecision: %s\n", decision.Verdict)
}
