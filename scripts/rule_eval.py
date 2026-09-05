"""Evaluation: would a hard decline rule on the recency check pay for itself?

Answers one question -- at what gap threshold, if any, is declining worthwhile --
by counting true and false positives against the labels. Ordering is by event
time then transaction id, so a gap is reproducible when two payments on a card
share a whole-second event time. Standard library only.

Usage:  python3 scripts/rule_eval.py data/fraudTrain.csv [data/fraudTest.csv ...]
"""

import csv
import sys
from collections import defaultdict
from datetime import datetime

CANDIDATE_THRESHOLDS = [1, 2, 3, 5, 10, 30, 60, 120, 300, 600]


def read_events_by_card(paths):
    """-> {card: [(unix_time, transaction_id, is_fraud), ...]}."""
    by_card = defaultdict(list)
    for path in paths:
        with open(path, newline="") as fh:
            for row in csv.DictReader(fh):
                if row.get("unix_time"):
                    ts = int(row["unix_time"])
                else:
                    ts = int(datetime.fromisoformat(
                        row["trans_date_trans_time"]).timestamp())
                by_card[row["cc_num"]].append(
                    (ts, row["trans_num"], row["is_fraud"] == "1"))
    return by_card


def gaps_with_labels(by_card):
    """-> [(gap_seconds, is_fraud), ...], one per scorable authorization.

    The first payment on a card has no previous payment and so no gap; it is
    not scorable by the rule and is excluded.
    """
    gaps = []
    for events in by_card.values():
        events.sort()                      # event time, then transaction id
        for i in range(1, len(events)):
            gaps.append((events[i][0] - events[i - 1][0], events[i][2]))
    return gaps


def humanize(seconds):
    for size, unit in ((86400, "d"), (3600, "h"), (60, "m")):
        if seconds >= size and seconds % size == 0:
            return f"{seconds // size}{unit}"
    return f"{seconds}s"


def main(paths):
    by_card = read_events_by_card(paths)
    gaps = gaps_with_labels(by_card)
    fraud = sum(1 for _, f in gaps if f)
    legit = len(gaps) - fraud

    print(f"{len(gaps):,} scorable authorizations across {len(by_card):,} cards")
    print(f"  fraud {fraud:,} ({100 * fraud / len(gaps):.3f}%)"
          f"   legitimate {legit:,}\n")

    print("Decline when the gap is under T")
    print(f"  {'T':>6}  {'declined':>9}  {'true pos':>8}  {'false pos':>10}"
          f"  {'precision':>9}  {'fraud recall':>12}  {'FP per TP':>9}")
    for t in CANDIDATE_THRESHOLDS:
        tp = sum(1 for gap, f in gaps if gap < t and f)
        fp = sum(1 for gap, f in gaps if gap < t and not f)
        declined = tp + fp
        precision = f"{tp / declined:.3f}" if declined else "n/a"
        ratio = f"{fp / tp:.0f}" if tp else "—"
        print(f"  {humanize(t):>6}  {declined:>9,}  {tp:>8,}  {fp:>10,}"
              f"  {precision:>9}  {100 * tp / fraud:>11.2f}%  {ratio:>9}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1:])
