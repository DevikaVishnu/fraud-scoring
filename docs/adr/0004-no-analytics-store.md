---
Status: accepted
---

# Kafka is offline-only, and there is no analytics store

Kafka sits behind the response, never on the path to it. It carries scored authorizations to durable history for retraining and replay. It does not serve the decision, and the velocity counters are not maintained through it — with only two counters and one timestamp lookup, a single fast store does that directly.

Druid was considered and rejected. Its distinguishing capability is sub-second arbitrary slicing over very large event volumes for interactive analysts. This project has no analysts. Retraining reads in bulk, audit is a lookup by id, and the ops dashboard is a few numbers per minute — Parquet files answer all three over this data volume instantly with nothing to operate. Adding Druid would demonstrate the ability to operate Druid and nothing about fraud. If an interactive analyst persona ever appears, revisit with ClickHouse first.