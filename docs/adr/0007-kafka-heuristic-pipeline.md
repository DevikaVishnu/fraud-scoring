---
Status: accepted
---

# A Kafka heuristic pipeline, explored alongside the scoring path

This records an alternative that was built and kept: a Kafka consumer that scores
authorizations with four hand-written rules and routes each one to an `approved`
or a `review` topic. It lives in `internal/heuristics`, `internal/stream`,
`internal/pipeline` and `cmd/pipeline`. It is **not** the decision path this repo
argues for. `cmd/score` is, and nothing here changes it.

It is recorded because it contradicts three accepted decisions, and an
unexplained contradiction in a codebase is worse than a wrong decision.

## What it contradicts

**[[0003-expected-cost-not-thresholds]] says decisions come from expected cost and
that reintroducing thresholds would be a regression.** Every rule here is a tuned
threshold. Four of them.

**[[0004-no-analytics-store]] says Kafka sits behind the response and never on the
path to it.** Here Kafka *is* the path: the pipeline consumes authorizations from
a topic and produces the routed result to another.

**[[0006-gap-is-a-signal-not-a-rule]] retired a gap threshold after measuring it as
net-harmful at every candidate value.** The `rapid_succession` rule is that same
threshold, at the same five seconds, brought back.

## Why it is defensible anyway

One difference carries the whole argument: **nothing here declines.**

ADR-0006's measurement is what makes this precise rather than hand-waving. At five
seconds the gap rule declines 488 authorizations to catch 14 frauds — 2.9%
precision, 34 false declines per catch. As a *decline* rule that is indefensible,
and ADR-0006 killed it correctly. As a *routing* rule the arithmetic changes,
because the cost of a false positive changes: a wrongly-routed authorization costs
an analyst a minute of attention, not a turned-away cardholder and the goodwill in
`config/costs.json`'s `false_decline_fixed`. A rule at five times base rate is
comfortably worth an analyst's minute. It was never worth a decline.

So the rules are kept, the thresholds are kept where they are visible in
`config/thresholds.json` with their evidence beside them, and the destination is a
queue rather than a verdict.

The Kafka placement is a genuine architectural difference rather than a
reinterpretation, and ADR-0004's reasoning still stands for the system it was
written about. A pipeline that routes to a review queue has no cardholder waiting
on it and no network deadline to meet, so the objection ADR-0004 raises — that a
broker must not sit between an authorization and its decision — does not apply to
this shape of work. Put the two together in one system and ADR-0004 wins.

## What this does not claim

The rules are not measured as a set. ADR-0006 measured the gap threshold alone,
against labels, and ADR-0002 established the velocity ceilings; the point totals,
the `review_at` cutoff and the amount threshold are chosen, and no run in this repo
supports them. A sweep of routing volume against caught fraud across plausible
threshold ranges is the obvious next measurement and has not been done.

The honest scope is the same as [[0001-deliberately-over-built]]: the pipeline
works end to end — consume, derive, assess, route, commit — and the routing volumes
it produces on simulated fraud are not evidence about real fraud.

## Consequences

**Signals are derived by `internal/signals`, not re-implemented.** The rules read
the same derived signals the model reads, so a rule cannot quietly disagree with
the model about what "the gap" means. The train/serve fixture protects this path
too, for free.

**Delivery is at-least-once, produce before commit.** A routed result reaches its
topic before the source offset is committed. A crash in between replays the
authorization and routes it twice; the other ordering loses it silently. A
duplicate in a review queue is a nuisance and an unscored payment is not, so the
ordering is the one that produces duplicates. `TestAMessageIsProducedBeforeItIsCommitted`
pins it.

**An authorization that cannot be scored is routed to review, not approved.** A
malformed message, a missing card, a negative amount: the failure is inside this
system, and a cardholder should not be approved by default because of it. This is
the same instinct as the scoring path's degradation to the base fraud rate, which
also fails toward caution rather than toward approval.

**A card's first payment has no gap, and the wire format says so by omission.**
`internal/signals` states a missing gap as NaN, which JSON cannot encode. The
routed message drops the key rather than sending a zero, because a consumer reading
a missing gap as a very short one is the exact mistake ADR-0006 is about.
