package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"github.com/DevikaVishnu/fraud-scoring/internal/stream"
)

// Demo authorizations are synthetic and say so. The cards and the event times
// are real -- taken from the checked-in history slice, so the velocity counters
// and the gap have something true to say -- but the amounts are invented,
// because that history carries no amount column. They are shaped to put at
// least one authorization through each of the four rules, so a demo run shows
// both topics receiving traffic rather than everything landing on one.
func demoAuthorizations(path string, perCard int) ([]stream.Authorization, error) {
	lastPayment, err := lastPaymentPerCard(path)
	if err != nil {
		return nil, err
	}
	cards := make([]string, 0, len(lastPayment))
	for card := range lastPayment {
		cards = append(cards, card)
	}
	sort.Strings(cards) // deterministic: the same demo run every time

	// offsets from the card's last known payment, and the amount to send.
	shapes := []struct {
		afterSeconds int64
		amountUSD    float64
		note         string
	}{
		{1, 42.00, "one second later: rapid succession"},
		{2, 1400.00, "two seconds later, large: rapid succession and large amount"},
		{900, 78.50, "fifteen minutes later: ordinary"},
		{6 * 3600, 310.00, "six hours later: ordinary, outside the hour window"},
	}

	var authorizations []stream.Authorization
	for _, card := range cards {
		for i := 0; i < perCard && i < len(shapes); i++ {
			shape := shapes[i]
			authorizations = append(authorizations, stream.Authorization{
				TransactionID: fmt.Sprintf("demo-%s-%d", card, i),
				Card:          card,
				EventTime:     lastPayment[card] + shape.afterSeconds,
				AmountUSD:     shape.amountUSD,
			})
		}
	}
	return authorizations, nil
}

func lastPaymentPerCard(path string) (map[string]int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening demo history %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("reading header of %s: %w", path, err)
	}
	card, eventTime := -1, -1
	for i, column := range header {
		switch column {
		case "cc_num":
			card = i
		case "unix_time":
			eventTime = i
		}
	}
	if card < 0 || eventTime < 0 {
		return nil, fmt.Errorf("%s: need cc_num and unix_time columns", path)
	}

	last := map[string]int64{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			return last, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		at, err := strconv.ParseInt(row[eventTime], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("reading %s: event time: %w", path, err)
		}
		if at > last[row[card]] {
			last[row[card]] = at
		}
	}
}
