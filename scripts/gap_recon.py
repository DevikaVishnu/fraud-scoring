"""Reconnaissance: how far apart are consecutive payments on the same card?

Answers one question -- which time windows are worth counting over -- by
looking at the data instead of guessing. Standard library only.

Usage:  python3 scripts/gap_recon.py data/fraudTrain.csv [data/fraudTest.csv ...]
"""

import csv
import sys
from collections import defaultdict
from datetime import datetime

CANDIDATE_WINDOWS = [10, 30, 60, 300, 900, 3600, 21600, 86400]
GAP_PERCENTILES = [0.1, 1, 5, 10, 25, 50, 75]


def read_times_by_card(paths):
    """-> {card: [(unix_time, is_fraud), ...]}, plus total row count."""
    by_card = defaultdict(list)
    rows = 0
    for path in paths:
        with open(path, newline="") as fh:
            for row in csv.DictReader(fh):
                rows += 1
                if row.get("unix_time"):
                    ts = int(row["unix_time"])
                else:
                    ts = int(datetime.fromisoformat(
                        row["trans_date_trans_time"]).timestamp())
                by_card[row["cc_num"]].append((ts, row["is_fraud"] == "1"))
    return by_card, rows


def percentile(sorted_values, pct):
    if not sorted_values:
        return None
    idx = min(len(sorted_values) - 1,
              max(0, round(pct / 100 * (len(sorted_values) - 1))))
    return sorted_values[idx]


def humanize(seconds):
    if seconds is None:
        return "n/a"
    for size, unit in ((86400, "d"), (3600, "h"), (60, "m")):
        if seconds >= size:
            return f"{seconds / size:.1f}{unit}"
    return f"{seconds:.0f}s"


def max_in_window(times, window):
    """Most payments falling inside any `window`-second span. Two pointers."""
    best = lo = 0
    for i, t in enumerate(times):
        while times[lo] < t - window:
            lo += 1
        best = max(best, i - lo + 1)
    return best


def main(paths):
    by_card, rows = read_times_by_card(paths)
    print(f"{rows:,} payments across {len(by_card):,} cards\n")

    all_gaps, fraud_gaps = [], []
    window_hits = {w: 0 for w in CANDIDATE_WINDOWS}
    window_max = {w: 0 for w in CANDIDATE_WINDOWS}

    for events in by_card.values():
        events.sort()
        times = [t for t, _ in events]
        for i in range(1, len(times)):
            gap = times[i] - times[i - 1]
            all_gaps.append(gap)
            if events[i][1]:
                fraud_gaps.append(gap)
        for w in CANDIDATE_WINDOWS:
            peak = max_in_window(times, w)
            window_max[w] = max(window_max[w], peak)
            if peak >= 2:
                window_hits[w] += 1

    all_gaps.sort()
    fraud_gaps.sort()

    print("Gap between consecutive payments on the same card")
    print(f"  {'pct':>6}  {'all':>10}  {'fraud only':>12}")
    for p in GAP_PERCENTILES:
        print(f"  {p:>5}%  {humanize(percentile(all_gaps, p)):>10}"
              f"  {humanize(percentile(fraud_gaps, p)):>12}")
    print(f"  {'min':>6}  {humanize(all_gaps[0]):>10}"
          f"  {humanize(fraud_gaps[0]) if fraud_gaps else 'n/a':>12}\n")

    total = len(by_card)
    print("Is a window worth counting over?")
    print(f"  {'window':>8}  {'cards w/ 2+ in window':>22}  {'busiest card':>13}")
    for w in CANDIDATE_WINDOWS:
        pct = 100 * window_hits[w] / total
        print(f"  {humanize(w):>8}  {window_hits[w]:>8,} ({pct:5.1f}%)"
              f"  {window_max[w]:>10} pmts")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit(__doc__)
    main(sys.argv[1:])
