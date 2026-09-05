---
Status: accepted
---

# Decisions come from expected cost, not score thresholds

There are no approve/decline thresholds in this system, and reintroducing them would be a regression.

For each authorization we compute the expected cost of all three decisions — approve, decline, challenge — from the calibrated risk score and the payment amount, and take the cheapest. Thresholds would need tuning, would need retuning per amount band, and would fit noise in whatever fraud happened to sit in each band. The expected-cost formula has no tuned parameters at all, and cutoff behaviour moves with the payment amount automatically, because the amount is already inside the cost of a missed fraud.

Two consequences worth knowing. The risk score **must** be calibrated — a gradient-boosted tree ranks rather than calibrates, so a held-out isotonic or Platt stage is mandatory; without it the arithmetic multiplies costs by a number that is not a probability, and this decision quietly degenerates into a tuned threshold with extra steps. And the timeout fallback needs no separate policy: run the same formula with the base fraud rate in place of a score, and it degrades in the right direction on its own.

The cost inputs are chosen, not measured, and no dataset here can supply them. They live in config, and the sensitivity sweep over plausible ranges is a required output — how much the decision mix moves is a more useful result than any single tuned number.