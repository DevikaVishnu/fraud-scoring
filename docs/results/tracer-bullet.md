# Tracer bullet: what came out of it

What one authorization travelling the whole path produced, and the two
measurements the ADRs require as outputs rather than as claims.

Everything below is measured on a held-out split of the simulated payment data:
the last 15% by event time, 277,860 authorizations, 924 of them fraud (0.333%).
The split is chronological because a random one would let a card's later
payments train a model that is then evaluated on its earlier ones.

**Scope, per [ADR-0001](../adr/0001-deliberately-over-built.md).** The plumbing
works end to end; that is the result. The fraud here is simulated, so none of
these numbers is evidence about real fraud. Read them as evidence that the
arithmetic is wired up and behaves the way the design says it should.

Reproduce with `make train` and `make sweep`.

## Is the risk score actually calibrated?

The whole expected-cost design rests on the risk score being a probability. A
gradient-boosted tree ranks rather than calibrates, so
[ADR-0003](../adr/0003-expected-cost-not-thresholds.md) makes a held-out
calibration stage mandatory. This is the check on it, not an assumption.

Ranking, for context: ROC AUC 0.9654, PR AUC 0.3488.

|                          | Brier score | expected calibration error |
| ------------------------ | ----------- | -------------------------- |
| raw model output         | 0.002682    | 0.001499                   |
| after the isotonic stage | 0.002615    | **0.000646**               |

Calibration error falls by more than half. The reliability table after
calibration, in deciles of predicted score:

| predicted range        |       n | mean predicted | observed rate |
| ---------------------- | ------: | -------------: | ------------: |
| [0.000000, 0.000020]   | 124,331 |       0.000015 |      0.000008 |
| [0.000020, 0.000139]   |  22,002 |       0.000139 |      0.000000 |
| [0.000139, 0.000266]   |  24,912 |       0.000241 |      0.000321 |
| [0.000266, 0.000300]   |  32,896 |       0.000300 |      0.000547 |
| [0.000300, 0.001241]   |  24,941 |       0.001052 |      0.001243 |
| [0.001241, 0.003824]   |  25,031 |       0.003116 |      0.002797 |
| [0.003824, 1.000000]   |  23,747 |       0.039951 |      0.033520 |

Predicted and observed track each other across four orders of magnitude, which
is what the arithmetic needs. The top bin is the one to watch: the model is
mildly over-confident there (0.0400 predicted against 0.0335 observed), which
biases the expected cost of approving upward and so errs toward intervening.
The isotonic stage collapses the bottom half of the range into long runs of
identical values, which is why the decile edges above are uneven.

## How far does the decision mix move across plausible costs?

The cost inputs are chosen, not measured, and no dataset here can supply them
(ADR-0003). So the reported result is the movement, not a tuned number. Each row
varies one input across a range someone could defend and holds the rest at their
configured values; `*` marks the configured value.

"Fraud stopped" is the share of fraud that was not approved.

At the configured costs in [`config/costs.json`](../../config/costs.json):

| approve | challenge | decline | fraud stopped |
| ------: | --------: | ------: | ------------: |
| 97.501% |    2.497% | 0.0018% |        74.03% |

| input                    | range swept      | approve           | challenge       | decline           | fraud stopped   |
| ------------------------ | ---------------- | ----------------- | --------------- | ----------------- | --------------- |
| `fraud_loss_multiple`    | 0.5 – 1.5 (\*1.0)| 98.00% → 96.99%   | 2.00% → 3.01%   | 0.0018%           | 73.05% → 75.22% |
| `chargeback_fee`         | 0 – 75 (\*25)    | 97.60% → 97.07%   | 2.40% → 2.93%   | 0.0018%           | 73.38% → 76.62% |
| `false_decline_fixed`    | 2 – 40 (\*8)     | 97.03% → 98.07%   | 2.96% → 1.93%   | 0.0090% → 0.0018% | 75.54% → 72.51% |
| `false_decline_rate`     | 0 – 0.10 (\*0.02)| 97.09% → 97.97%   | 2.90% → 2.03%   | 0.0108% → 0.0018% | 74.89% → 73.27% |
| `challenge_cost`         | 0.25 – 8 (\*1.5) | 96.78% → 98.18%   | 3.22% → 1.74%   | 0.0018% → 0.0795% | 76.73% → 71.97% |
| `challenge_abandon_rate` | 0.02 – 0.5(\*0.12)| 96.89% → 98.14%  | 3.11% → 1.85%   | 0.0018% → 0.0029% | 75.65% → 72.62% |

Three things are worth taking from this.

**The mix is stable.** Across every input's full plausible range the challenge
share stays between 1.7% and 3.2%, and fraud stopped between 72% and 77%. A
sixteen-fold swing in the fixed cost of a false decline moves fraud stopped by
three points. Nothing here is balanced on a knife edge, which is the reassuring
answer -- and it is an answer that a single tuned number could not have given.

**All three decisions are reachable, but decline is rare.** At the configured
costs, declining wins on five of 277,860 authorizations. That is arithmetic
working correctly rather than a bug: a challenge shifts liability to the bank
when a fraudster passes it, so it dominates declining unless the risk score is
very high or challenging is expensive. Decline's share grows with
`challenge_cost` -- at $8 it is forty times its configured share -- which is
exactly the relationship the formula should have.

**The cutoff moves with the amount on its own.** The payment amount is inside
the cost of a missed fraud, so no per-amount-band tuning appears anywhere in the
sweep, or anywhere in the code. There are no approve/decline thresholds to tune;
reintroducing any would be the regression ADR-0003 warns about.

## What the tracer bullet did not settle

- The velocity counters are populated from the checked-in payment files at
  startup. That stands in for the background process ADR-0002 describes, and
  replacing it is additive.
- Durable history is a Parquet file written behind the response. Putting Kafka
  in front of it, as [ADR-0004](../adr/0004-no-analytics-store.md) has it,
  touches `internal/history` and nothing else.
- Nothing here is a service. The entry point's shape is the shape a request
  handler would call, but no handler exists yet.
