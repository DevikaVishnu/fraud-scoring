---
Status: accepted
---

# The five-second decline rule is retired; the gap is a model signal

[[0002-per-signal-freshness]] specified a hard rule declining an authorization less
than five seconds after the previous payment on the same card, and flagged the
threshold as unsupported by its own evidence. Measured against the labels, the rule
is net-harmful at five seconds and at every other candidate value. It is removed.
The gap the recency check reads becomes an ordinary signal into the model, and the
decision comes from expected cost like every other decision.

## Evidence

`scripts/rule_eval.py` over `fraudTrain.csv` + `fraudTest.csv`, ordering by event
time then transaction id. 1,851,395 scorable authorizations across 999 cards, 9,560 fraud (0.516%).
A payment is declined by the rule when its gap is under T.

| T    | declined | true pos | false pos | precision | fraud recall | false declines per catch |
| ---- | -------- | -------- | --------- | --------- | ------------ | ------------------------ |
| 1s   | 44       | 0        | 44        | 0.000     | 0.00%        | —                        |
| 2s   | 133      | 2        | 131       | 0.015     | 0.02%        | 66                       |
| 3s   | 258      | 5        | 253       | 0.019     | 0.05%        | 51                       |
| 5s   | 488      | 14       | 474       | 0.029     | 0.15%        | 34                       |
| 10s  | 998      | 27       | 971       | 0.027     | 0.28%        | 36                       |
| 30s  | 3,057    | 77       | 2,980     | 0.025     | 0.81%        | 39                       |
| 1m   | 6,311    | 137      | 6,174     | 0.022     | 1.43%        | 45                       |
| 5m   | 30,798   | 631      | 30,167    | 0.020     | 6.60%        | 48                       |
| 10m  | 60,746   | 1,158    | 59,588    | 0.019     | 12.11%       | 51                       |

At the specified five seconds the rule declines 488 legitimate-heavy authorizations
to catch 14 frauds, missing 99.85% of fraud. At one second it declines 44 and
catches none.

The decisive number is not any single row but the precision column: it sits between
1.9% and 2.9% across three orders of magnitude of T, never rising above roughly five
times the 0.516% base rate. There is no threshold at which this rule becomes good.
Lowering T concentrates nothing; raising it buys recall at flat precision. A cutoff
choice cannot rescue it because the gap carries the same weak lift everywhere.

## Why a signal instead

The lift is real — five times base rate is worth having — so the information stays
and only the rule goes. Feeding the gap to the model preserves it and hands the
decision to the expected-cost formula, which is where [[0003-expected-cost-not-thresholds]]
says it belongs. A hard decline rule is a tuned threshold wearing different clothes,
and it is one the formula cannot see around: at 2.9% precision, declining is the
right call only when the payment amount makes it so, and a fixed T cannot express
that condition. The formula can, and already does, because the amount is inside the
cost of a missed fraud.

## Consequences

**The live path loses its last exact-read consumer.** The recency check was read
exactly on the critical path solely to serve this rule. As a model signal the gap
tolerates the same lag the velocity counters do, so the single-timestamp-per-card
live store moves to the background with them and the critical path fetches nothing
exactly. The per-signal freshness decision in [[0002-per-signal-freshness]] is
unchanged as a principle — freshness is still decided per signal against that
signal's own window — but it now has zero exact-read instances. A future signal may
reintroduce one; none currently demands it.

**Payments are ordered by event time then transaction id.** Whole-second timestamp
resolution puts genuinely distinct payments at identical event times, which leaves
"the previous payment" undefined and makes the gap non-reproducible on replay. The
transaction id as secondary sort key restores determinism. This was worth doing
regardless of how the rule resolved, and survives the rule's removal because the
gap is still computed, just for the model rather than for a decline.
