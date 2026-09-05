package signals

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
)

// A Store holds each card's payment history, keyed on the card because the
// recon established that one card belongs to one cardholder here, so a counter
// cannot pick up a second person's payments.
//
// It is read on the scoring path and populated off it. Populating it from the
// checked-in payment files, as LoadCSV does, stands in for the background
// process that will maintain it later: the counters are permitted to lag, and
// nothing on the critical path is read exactly (ADR-0002, ADR-0006).
type Store struct {
	timelines map[string]Timeline
}

func NewStore() *Store {
	return &Store{timelines: map[string]Timeline{}}
}

// History is the card's payments in key order. An unknown card has none, which
// is the same answer as a card whose first payment is being scored.
func (s *Store) History(card string) Timeline { return s.timelines[card] }

func (s *Store) Cards() int { return len(s.timelines) }

// Record adds one payment. Callers add in any order and Sort afterwards.
func (s *Store) Record(card string, payment Payment) {
	s.timelines[card] = append(s.timelines[card], payment)
}

// Sort puts every timeline into key order. Derive depends on it.
func (s *Store) Sort() {
	for _, timeline := range s.timelines {
		sort.Sort(timeline)
	}
}

// LoadCSV fills a store from the payment files the training side reads, so both
// sides derive signals from the same history.
func LoadCSV(store *Store, paths ...string) error {
	for _, path := range paths {
		if err := loadOne(store, path); err != nil {
			return err
		}
	}
	store.Sort()
	return nil
}

func loadOne(store *Store, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening payments %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.ReuseRecord = true
	header, err := reader.Read()
	if err != nil {
		return fmt.Errorf("reading header of %s: %w", path, err)
	}
	columns, err := columnIndexes(header, "cc_num", "trans_num", "unix_time")
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	for line := 2; ; line++ {
		row, err := reader.Read()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s line %d: %w", path, line, err)
		}
		eventTime, err := strconv.ParseInt(row[columns[2]], 10, 64)
		if err != nil {
			return fmt.Errorf("reading %s line %d: event time: %w", path, line, err)
		}
		store.Record(row[columns[0]], Payment{
			TransactionID: row[columns[1]],
			EventTime:     eventTime,
		})
	}
}

func columnIndexes(header []string, wanted ...string) ([]int, error) {
	indexes := make([]int, len(wanted))
	for i, name := range wanted {
		indexes[i] = -1
		for j, column := range header {
			if column == name {
				indexes[i] = j
			}
		}
		if indexes[i] < 0 {
			return nil, fmt.Errorf("no %q column", name)
		}
	}
	return indexes, nil
}
