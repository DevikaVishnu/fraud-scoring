---
Status: accepted
---

# Signal freshness is decided per signal, not globally

Velocity counters lag; the recency check does not. This looks inconsistent and is deliberate.

Freshness only matters relative to a signal's own time window. The velocity counters run over one hour and twenty-four hours, so a few seconds of lag is a rounding error and they are maintained in the background. The recency check — the gap since the card's previous payment — is read exactly on the critical path, because the rule it feeds cannot tolerate staleness.

## Evidence

`scripts/gap_recon.py` over `fraudTrain.csv` + `fraudTest.csv`: 1,852,394 payments across 999 cards.

| window | cards with 2+ inside | busiest card |
| ------ | -------------------- | ------------ |
| 10s    | 496 (49.6%)          | 3 payments   |
| 30s    | 690 (69.1%)          | 3 payments   |
| 1m     | 797 (79.8%)          | 3 payments   |
| 5m     | 941 (94.2%)          | 4 payments   |
| 15m    | 978 (97.9%)          | 5 payments   |
| 1h     | 996 (99.7%)          | 9 payments   |
| 6h     | 999 (100.0%)         | 20 payments  |
| 1d     | 999 (100.0%)         | 36 payments  |

Every window below five minutes saturates at three payments. A counter with a range of 0-3 carries almost no information regardless of how fresh it is, which is what disqualifies sub-minute windows — not, as an earlier draft of this record claimed, an absence of short-gap activity. Short gaps are common: four cards in five have two payments inside a minute at some point. The hour and day windows were chosen because they are the shortest with usable range (0-9 and 0-36). Six hours (0-20) is an equally defensible third.

Since both chosen windows are hours long, a few seconds of counter lag is immaterial, and every velocity counter moved to the background. The live subsystem collapsed to a single timestamp per card. Keep it that small: it exists because a rule demands exactness, not because a model wanted freshness.

## Resolved: the five-second threshold did not survive measurement

The per-signal freshness decision stands. The specific threshold on the rule it
served did not: measured against the labels, the rule was net-harmful at five
seconds and at every candidate value, and it is retired in
[[0006-gap-is-a-signal-not-a-rule]]. The gap is now a model signal rather than a
decline rule.

Gap percentiles from the same run, all payments against fraud only:

| pct   | all    | fraud  | ratio |
| ----- | ------ | ------ | ----- |
| 0.1%  | 17s    | 3s     | 5.7x  |
| 1%    | 3.0m   | 39s    | 4.6x  |
| 5%    | 15.5m  | 3.7m   | 4.2x  |
| 10%   | 32.5m  | 7.8m   | 4.2x  |
| 25%   | 1.6h   | 24.4m  | 3.9x  |
| 50%   | 4.4h   | 1.4h   | 3.1x  |
| 75%   | 10.6h  | 8.0h   | 1.3x  |
| min   | 0s     | 1s     | —     |

Fraud gaps are tighter at every percentile, so timing is a real signal — but the
separation is concentrated in the tail and has nearly vanished by the upper
quartile. That is what the rule foundered on: fraud's 0.1th percentile is 3s, so
gaps under five seconds occupy only the extreme tail of fraud, while the minimum
gap is 0s across all payments and 1s among fraud, meaning every zero-gap pair in
this data is legitimate traffic — precisely what the rule would decline first.
[[0006-gap-is-a-signal-not-a-rule]] carries the true and false positive counts.

One correction to the reasoning above, from the same measurement. The zero-gap
pairs are not duplicated or replayed rows: every colliding pair differs in
transaction id, merchant and amount, and event times are whole seconds throughout
the data. They are distinct payments collapsed by timestamp resolution, which is
why ordering now takes transaction id as a secondary key.

Because the rule is gone, the live subsystem described above has no remaining
consumer for an exactly-read signal, and the single timestamp per card joins the
velocity counters in the background. The principle is unaffected; it simply has no
exact-read instance at present.
