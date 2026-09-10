# fraud-detection-pipeline

Real-time risk scoring for card authorization attempts. An authorization
arrives, a risk score is computed from its card's history, and a decision --
approve, decline or challenge -- is returned to the caller.

The vocabulary is in [`CONTEXT.md`](CONTEXT.md); the decisions behind the design
are in [`docs/adr/`](docs/adr). This is a demonstration project and is
[deliberately over-built](docs/adr/0001-deliberately-over-built.md).

## See it work

```
make score
```

That scores one authorization through every layer -- signal derivation, model,
calibration, expected-cost arithmetic -- and prints the decision with everything
it was computed from, so the verdict can be recomputed by hand. It needs nothing
but a clone: the model, the calibration table and a small slice of card history
are checked in.

```
make test
```

runs the Go suite, including the train/serve consistency fixture described below.

## What this is

A tracer bullet: one authorization in, one decision out, through every layer the
real system will have, at the thinnest possible width.

Nothing here is wide, on purpose. Two velocity counters, not a signal library.
One model, no experiment tracking. One entry point, no HTTP surface, no service,
no Kafka. The effort went into the joints between the layers, because those are
the parts that are expensive to change later.

```
scoring/            the entry point, and the only public surface
cmd/score/          one command: an authorization in, a decision printed
internal/signals/   the recency check gap and the two velocity counters
internal/risk/      the LightGBM reader and the calibration stage
internal/decisioning/  the expected-cost arithmetic
internal/history/   durable history, written behind the response
internal/heuristics/   the four rules explored in ADR-0007
internal/stream/       Kafka in and out, and the in-process fake
internal/pipeline/     the consume-assess-route-commit loop
cmd/pipeline/          the Kafka pipeline: authorizations in, two topics out
training/           Python: trains the model, exports artifacts and the fixture
artifacts/          model.txt and calibration.json, loaded once at startup
testdata/           the train/serve fixture
scripts/            the two reconnaissance runs behind ADR-0002 and ADR-0006
```

`scoring` is the only package outside `internal/`, so a caller -- and every test
in this repo -- reaches behaviour and cannot reach the parts. Its shape is the
shape a request handler would call, so putting a service in front of it later is
a wrapping job rather than a rewrite.

## The joint that gets first-class treatment

Two languages derive the same signals from the same definitions: Python for
training, Go for serving. Nothing makes them agree by construction, and when
they drift the result silently degrades rather than failing loudly. That is the
failure mode this design is most exposed to.

So [`testdata/train_serve_fixture.json`](testdata/train_serve_fixture.json) is a
required deliverable. It holds authorizations, the signals the training side
derived for them, and the calibrated risk scores it produced. The Go test
`TestServingDerivesTheSameSignalsAsTraining` replays them through the serving
path and asserts agreement: signals exactly, so an off-by-one in a window
boundary or an ordering tie-break cannot hide inside a tolerance; risk scores
within a stated tolerance, because two runtimes summing the same trees will not
agree bit for bit.

It pins the cases that are easy to get wrong: a card's first payment, two
payments sharing a whole-second event time, and payments sitting exactly on each
velocity window boundary.

Regenerate it with `make train`. A change to it is a change in signal semantics
and should be read as one in review.

## The Kafka heuristic pipeline

A second, separate path explored on this branch and recorded in
[ADR-0007](docs/adr/0007-kafka-heuristic-pipeline.md): a Kafka consumer that
scores authorizations with four hand-written rules and routes each one to an
`approved` or a `review` topic.

```
make pipeline-demo
```

runs the whole loop with no broker at all -- an in-process fake stands in for
Kafka -- so it works on a fresh clone the way `make score` does. Against a real
broker:

```
make pipeline-up     # single-broker Kafka in Docker, KRaft, no ZooKeeper
make pipeline-seed   # fill the input topic with synthetic authorizations
make pipeline        # consume, score, route
make pipeline-down
```

The four rules are `rapid_succession`, `hourly_velocity`, `daily_velocity` and
`large_amount`. Each carries points; the total decides the topic. Thresholds are
in [`config/thresholds.json`](config/thresholds.json) with their evidence beside
them -- two are grounded in this repo's reconnaissance runs and two are chosen,
and the file says which is which.

**This is not the decision path this repo argues for**, and it contradicts three
accepted ADRs: thresholds instead of expected cost (ADR-0003), Kafka on the path
rather than behind it (ADR-0004), and the revival of the very gap threshold
ADR-0006 measured as net-harmful. ADR-0007 exists to say so and to give the one
argument that makes it defensible: nothing here declines. A rule at five times
base rate is worth an analyst's minute, which is what routing to a queue costs.
It was never worth a false decline, which is what ADR-0006 killed it for.

The rules read signals derived by `internal/signals` -- the same derivation the
model reads -- so a rule cannot quietly disagree with the model about what "the
gap" means.

## Retraining

Training reads the two Kaggle "Sparkov" simulated card payment files, which are
478MB and are not checked in. Put them at `data/fraudTrain.csv` and
`data/fraudTest.csv`, then:

```
make venv     # once
make train    # retrains, re-exports every artifact, regenerates the fixture
make sweep    # how far the decision mix moves across plausible cost inputs
make recon    # the two reconnaissance runs behind the ADRs
```

`make train` is reproducible from those files with one command. What it writes
is in [`docs/results/tracer-bullet.md`](docs/results/tracer-bullet.md), along
with the held-out calibration measurement and the cost sensitivity sweep.

The small slice of card history the fixture replays against
(`data/fixture_payments.csv`) is checked in, which is why `make score` and
`make test` work without the download.

## Scope

Per [ADR-0001](docs/adr/0001-deliberately-over-built.md): the honest result here
is that the plumbing works end to end. The fraud in this data is simulated, so
detection quality measured on it is not evidence about real fraud.
