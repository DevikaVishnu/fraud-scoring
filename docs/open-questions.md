# Open questions

Raised by the first run of `scripts/gap_recon.py` (1,852,394 payments, 999 cards).
All three are now settled. They were ordered by dependency: the tie-break gated the
threshold, so it was answered first. Resolutions are recorded here and the decision
they produced is [[0006-gap-is-a-signal-not-a-rule]].

## 1. A minimum gap of 0s leaves "the previous payment" undefined — RESOLVED

**Timestamp granularity, not a data artifact.** Of the three candidate causes —
whole-second granularity collapsing two distinct payments, a duplicated row, or a
replayed record — the first is correct and the other two are ruled out.

44 colliding keys, 88 rows. Every colliding pair differs in transaction id,
merchant, amount and merchant coordinates, and 36 of the 44 differ in category as
well. Not one pair is identical in its other columns, so neither duplication nor
replay explains any of them. `trans_date_trans_time` carries whole seconds in all
1,852,394 rows, with no sub-second value anywhere in the data. These are genuinely
distinct payments landing in the same second.

**The tie-break is adopted.** Ordering is by event time then transaction id. The
resolution of question 2 removed the hard rule but not the gap, so replay
determinism still matters and the secondary key still earns its place.

## 2. The five-second threshold — RESOLVED, the rule is retired

Measured across 1,851,395 scorable authorizations, the rule declines 488 to catch
14 frauds at five seconds, and 44 to catch none at one second. Precision stays
between 1.9% and 2.9% from one second to ten minutes — never more than about five
times the 0.516% base rate — so no threshold makes the rule worthwhile. It is
removed in [[0006-gap-is-a-signal-not-a-rule]] and the gap becomes a model signal,
with the decision left to the expected-cost formula of
[[0003-expected-cost-not-thresholds]]. Full table in the ADR.

The downstream reach landed as anticipated: with the rule gone the live path has no
consumer for an exactly-read signal, and the single timestamp per card moves to the
background alongside the velocity counters.

## 3. 999 distinct cards, where the dataset describes 1,000 customers — RESOLVED, benign

999 distinct cards and 999 distinct cardholders, mapping one to one in both
directions: no card belongs to two people and no person holds two cards. The
candidate that would have mattered — one card shared by two customers, making card
unsafe to key velocity counters on — is eliminated. **Card is a safe identity for
velocity counters.**

What remains is the discrepancy itself, which is a rounding error in the recon
tables and nothing more: the thousandth customer has no payments in the loaded
range. Every per-card percentage normalises by 999.
