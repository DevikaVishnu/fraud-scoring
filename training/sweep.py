"""Sensitivity sweep: how far does the decision mix move across plausible costs?

The cost inputs are chosen, not measured, and no dataset here can supply them
(ADR-0003). So the useful result is not the mix at one setting but how much the
mix moves when each input is varied across a range someone could defend. This
script is that measurement.

It reads the exported artifacts rather than retraining, so it reports on the
same model the serving side loads.

Usage:  python training/sweep.py [--data data/fraudTrain.csv data/fraudTest.csv]
"""

import argparse
import json
import pathlib
import sys

import lightgbm as lgb
import numpy as np

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import signals as sig  # noqa: E402
from train import (CALIBRATION_PATH, DEFAULT_DATA, MODEL_PATH, ROOT,  # noqa: E402
                   chronological_split)

COSTS_PATH = ROOT / "config" / "costs.json"
VERDICTS = ["approve", "challenge", "decline"]

# Ranges someone could defend, around each configured value.
SWEEPS = {
    "fraud_loss_multiple": [0.5, 0.75, 1.0, 1.25, 1.5],
    "chargeback_fee": [0.0, 15.0, 25.0, 40.0, 75.0],
    "false_decline_fixed": [2.0, 5.0, 8.0, 15.0, 40.0],
    "false_decline_rate": [0.0, 0.01, 0.02, 0.05, 0.10],
    "challenge_cost": [0.25, 0.75, 1.5, 3.0, 8.0],
    "challenge_abandon_rate": [0.02, 0.06, 0.12, 0.25, 0.50],
}


def load_costs():
    costs = json.loads(COSTS_PATH.read_text())
    return {key: value for key, value in costs.items()
            if isinstance(value, (int, float))}


def apply_calibration(table, raw):
    """The same piecewise-linear, clipped isotonic stage the serving side applies."""
    return np.interp(raw, table["x"], table["y"],
                     left=table["y"][0], right=table["y"][-1])


def decide(costs, risk_scores, amounts):
    """-> the verdict for every authorization, as the serving side would pick it.

    Deliberately the same arithmetic as internal/decisioning, restated here
    because this script has to sweep over it. It is not on the decision path.
    """
    fraud, legitimate = risk_scores, 1 - risk_scores
    false_decline = costs["false_decline_fixed"] + costs["false_decline_rate"] * amounts

    expected = np.stack([
        fraud * (amounts * costs["fraud_loss_multiple"] + costs["chargeback_fee"]),
        costs["challenge_cost"] + legitimate * costs["challenge_abandon_rate"] * false_decline,
        legitimate * false_decline,
    ])
    # Ties go to the least intervention, which is the order of VERDICTS.
    return np.argmin(expected, axis=0)


def mix(costs, risk_scores, amounts):
    """-> {verdict: share of authorizations}, as percentages."""
    chosen = decide(costs, risk_scores, amounts)
    return {verdict: 100 * float((chosen == i).mean())
            for i, verdict in enumerate(VERDICTS)}


def caught(costs, risk_scores, amounts, labels):
    """-> share of fraud not approved, as a percentage."""
    chosen = decide(costs, risk_scores, amounts)
    return 100 * float((chosen[labels == 1] != 0).mean())


def print_row(label, shares, fraud_caught):
    print(f"  {label:>22}  " + "  ".join(f"{shares[v]:>9.4f}%" for v in VERDICTS)
          + f"  {fraud_caught:>13.2f}%")


def main(data_paths):
    costs = load_costs()
    booster = lgb.Booster(model_file=str(MODEL_PATH))
    table = json.loads(CALIBRATION_PATH.read_text())

    payments = sig.derive(sig.load_payments(data_paths))
    _, _, evaluate = chronological_split(payments)
    risk_scores = apply_calibration(table, booster.predict(sig.signal_matrix(evaluate)))
    amounts = evaluate["amount"].to_numpy()
    labels = evaluate["is_fraud"].to_numpy()

    print(f"{len(evaluate):,} held-out authorizations, "
          f"{int(labels.sum()):,} of them fraud ({100 * labels.mean():.3f}%)\n")
    header = f"  {'setting':>22}  " + "  ".join(f"{v:>10}" for v in VERDICTS) + \
             f"  {'fraud stopped':>14}"

    print("At the configured costs")
    print(header)
    print_row("as configured", mix(costs, risk_scores, amounts),
              caught(costs, risk_scores, amounts, labels))

    for name, values in SWEEPS.items():
        print(f"\nVarying {name} (configured: {costs[name]:g})")
        print(header)
        for value in values:
            varied = dict(costs, **{name: value})
            marker = " *" if value == costs[name] else ""
            print_row(f"{value:g}{marker}", mix(varied, risk_scores, amounts),
                      caught(varied, risk_scores, amounts, labels))

    print("\nScope: this is simulated fraud. Per ADR-0001, what is worth reading "
          "here is how far the mix moves, not any single number.")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", nargs="+", type=pathlib.Path, default=DEFAULT_DATA)
    main(parser.parse_args().data)
