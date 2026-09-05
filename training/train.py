"""Train the model, export the artifacts, and emit the train/serve fixture.

Python trains and never sees a live authorization (ADR-0005). What leaves this
script is three files:

  artifacts/model.txt                  the LightGBM model, read in-process by Go
  artifacts/calibration.json           the held-out isotonic stage, as a table
  testdata/train_serve_fixture.json    authorizations, signals and risk scores,
                                       replayed by the Go side and asserted

Usage:  python training/train.py [--data data/fraudTrain.csv data/fraudTest.csv]
"""

import argparse
import json
import pathlib
import sys

import lightgbm as lgb
import numpy as np
from sklearn.isotonic import IsotonicRegression
from sklearn.metrics import average_precision_score, roc_auc_score

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import signals as sig  # noqa: E402

ROOT = pathlib.Path(__file__).resolve().parent.parent
DEFAULT_DATA = [ROOT / "data" / "fraudTrain.csv", ROOT / "data" / "fraudTest.csv"]

MODEL_PATH = ROOT / "artifacts" / "model.txt"
CALIBRATION_PATH = ROOT / "artifacts" / "calibration.json"
FIXTURE_PATH = ROOT / "testdata" / "train_serve_fixture.json"
FIXTURE_HISTORY_PATH = ROOT / "data" / "fixture_payments.csv"

# How much of a card's history the serving side needs to reproduce a case:
# the whole longest velocity window, plus a little after it, so that a fixture
# also pins the fact that an authorization counts neither itself nor anything
# later than itself.
HISTORY_BEFORE = max(sig.VELOCITY_WINDOWS.values())
HISTORY_AFTER = 3600

SEED = 20260905
# Chronological, not random: a random split would let a card's later payments
# train a model that is then evaluated on its earlier ones.
TRAIN_FRACTION, CALIBRATION_FRACTION = 0.70, 0.15

# Two runtimes evaluating the same trees will not agree bit for bit.
RISK_SCORE_TOLERANCE = 1e-9

LGB_PARAMS = {
    "objective": "binary",
    "learning_rate": 0.05,
    "num_leaves": 31,
    "min_data_in_leaf": 200,
    "feature_fraction": 1.0,
    "bagging_fraction": 0.8,
    "bagging_freq": 1,
    "verbose": -1,
    "seed": SEED,
    "deterministic": True,
    "force_row_wise": True,
    "num_threads": 1,
}
NUM_ROUNDS = 300


def chronological_split(payments):
    """-> (train, calibration, evaluation), split by event time.

    Calibration is fitted on rows the tree never trained on, so it corrects the
    tree's overconfidence rather than memorising it (ADR-0003).
    """
    order = np.argsort(payments["event_time"].to_numpy(), kind="mergesort")
    ordered = payments.iloc[order]
    n = len(ordered)
    train_end = int(n * TRAIN_FRACTION)
    calibration_end = int(n * (TRAIN_FRACTION + CALIBRATION_FRACTION))
    return (ordered.iloc[:train_end],
            ordered.iloc[train_end:calibration_end],
            ordered.iloc[calibration_end:])


def calibration_table(calibrator):
    """-> the isotonic stage as a lookup table, small enough to read by eye."""
    return {
        "kind": "isotonic",
        "comment": ("Piecewise-linear between knots, clipped to the end values "
                    "outside them. Fitted on a held-out split."),
        "x": [float(v) for v in calibrator.X_thresholds_],
        "y": [float(v) for v in calibrator.y_thresholds_],
    }


def reliability(probabilities, labels, bins=10):
    """-> [(lo, hi, count, mean predicted, observed rate), ...] over `bins` deciles.

    The central assumption of expected-cost decisioning is that the risk score is
    a probability. This is the check on it, and it is a check on simulated fraud:
    ADR-0001 says detection quality here is not evidence about real fraud.

    Bins are disjoint and are deduplicated first: a calibrated score has long
    runs of identical values, and quantile edges over those collapse together.
    """
    edges = np.unique(np.quantile(probabilities, np.linspace(0, 1, bins + 1)))
    assigned = np.clip(np.searchsorted(edges, probabilities, side="left") - 1,
                       0, len(edges) - 2)
    rows = []
    for b in range(len(edges) - 1):
        inside = assigned == b
        if not inside.any():
            continue
        rows.append((edges[b], edges[b + 1], int(inside.sum()),
                     float(probabilities[inside].mean()),
                     float(labels[inside].mean())))
    return rows


def expected_calibration_error(rows, total):
    return sum(count * abs(predicted - observed)
               for _, _, count, predicted, observed in rows) / total


def report_calibration(name, probabilities, labels):
    rows = reliability(probabilities, labels)
    brier = float(np.mean((probabilities - labels) ** 2))
    ece = expected_calibration_error(rows, len(labels))
    print(f"\n{name} on the held-out split "
          f"(Brier {brier:.6f}, expected calibration error {ece:.6f})")
    print(f"  {'bin':>22}  {'n':>8}  {'mean predicted':>14}  {'observed':>10}")
    for lo, hi, count, predicted, observed in rows:
        print(f"  [{lo:.6f}, {hi:.6f}]  {count:>8,}  {predicted:>14.6f}"
              f"  {observed:>10.6f}")
    return brier, ece


def mark_window_boundaries(payments):
    """Flag rows with a predecessor sitting exactly on a velocity window edge.

    Whether that payment is inside the counter or outside it is the inclusive /
    exclusive choice, and it is the kind of thing that is easy to change by
    accident in one language and not the other. Must be run while the frame is
    still in card-then-event-time order.
    """
    payments = payments.copy()
    for name, window in sig.VELOCITY_WINDOWS.items():
        payments[f"on_{name}_boundary"] = _boundary_mask(payments, window)
    return payments


def _boundary_mask(payments, window):
    mask = np.zeros(len(payments), dtype=bool)
    for start, stop in sig.card_spans(payments["card"].to_numpy()):
        times = payments["event_time"].to_numpy()[start:stop]
        edge = np.searchsorted(times, times - window, side="left")
        index = np.arange(len(times))
        mask[start:stop] = (edge < index) & (times[edge] == times - window)
    return mask


def pick_fixture_cases(scored):
    """-> rows pinning the cases where signal derivation is easy to get wrong.

    A card's first payment, two payments sharing a whole-second event time, and a
    payment sitting exactly on a velocity window boundary. Plus a spread of
    ordinary authorizations across the range of risk, so the fixture also covers
    the path everything else takes.
    """
    chosen = {}

    def take(name, candidates):
        if len(candidates):
            chosen[name] = candidates.iloc[0]

    take("boundary-first-payment-on-card",
         scored[scored["gap_seconds"].isna()])
    take("boundary-same-second-tie-break",
         scored[scored["gap_seconds"] == 0])
    for name in sig.VELOCITY_WINDOWS:
        take(f"boundary-exactly-on-{name}", scored[scored[f"on_{name}_boundary"]])
    take("busy-card-high-velocity",
         scored.sort_values(["velocity_24h", "transaction_id"],
                            ascending=[False, True]))

    ordinary = scored[scored["gap_seconds"].notna()].sort_values("risk_score")
    for i, quantile in enumerate([0.01, 0.25, 0.50, 0.75, 0.95, 0.999]):
        row = ordinary.iloc[min(len(ordinary) - 1, int(quantile * len(ordinary)))]
        chosen[f"ordinary-{i}-risk-p{quantile:g}"] = row

    return chosen


def write_fixture_history(payments, chosen):
    """Write the slice of card history the serving side needs to replay the cases.

    Checked in, in the same column shape as the raw payment files, so the
    fixture stands on its own: replaying it needs no 478MB download.
    """
    wanted = set()
    spans = {}
    for start, stop in sig.card_spans(payments["card"].to_numpy()):
        for position in range(start, stop):
            spans[position] = (start, stop)

    times = payments["event_time"].to_numpy()
    for row in chosen.values():
        position = int(row.name)
        start, stop = spans[position]
        at = times[position]
        inside = np.flatnonzero((times[start:stop] >= at - HISTORY_BEFORE)
                                & (times[start:stop] <= at + HISTORY_AFTER)) + start
        wanted.update(int(p) for p in inside)
        if position > start:
            wanted.add(position - 1)

    history = payments.iloc[sorted(wanted)][
        ["card", "transaction_id", "event_time"]]
    history = history.rename(columns={"card": "cc_num",
                                      "transaction_id": "trans_num",
                                      "event_time": "unix_time"})
    history.to_csv(FIXTURE_HISTORY_PATH, index=False)
    return len(history)


def write_fixture(chosen):
    def signal_value(row, name):
        value = float(row[name])
        # JSON has no NaN. A missing gap is null on both sides of the fixture.
        return None if np.isnan(value) else value

    cases = [{
        "name": name,
        "authorization": {
            "transaction_id": row["transaction_id"],
            "card": row["card"],
            "event_time": int(row["event_time"]),
            "amount_usd": float(row["amount"]),
        },
        "signals": {name: signal_value(row, name) for name in sig.SIGNAL_SET},
        "risk_score": float(row["risk_score"]),
    } for name, row in sorted(chosen.items())]

    FIXTURE_PATH.write_text(json.dumps({
        "comment": ("Produced by the training side. The serving side replays "
                    "these authorizations and must agree. Regenerate with "
                    "`make train`; a change here is a change in signal "
                    "semantics and should be read as one."),
        "signal_set": sig.SIGNAL_SET,
        "velocity_windows_seconds": sig.VELOCITY_WINDOWS,
        "risk_score_tolerance": RISK_SCORE_TOLERANCE,
        "cases": cases,
    }, indent=2) + "\n")


def main(data_paths):
    print(f"reading {', '.join(str(p) for p in data_paths)}")
    payments = mark_window_boundaries(sig.derive(sig.load_payments(data_paths)))
    fraud = int(payments["is_fraud"].sum())
    print(f"{len(payments):,} payments across {payments['card'].nunique():,} cards"
          f"   fraud {fraud:,} ({100 * fraud / len(payments):.3f}%)")

    train, calibrate, evaluate = chronological_split(payments)
    print(f"split by event time: train {len(train):,}"
          f"   calibrate {len(calibrate):,}   evaluate {len(evaluate):,}")

    booster = lgb.train(
        LGB_PARAMS,
        lgb.Dataset(sig.signal_matrix(train), label=train["is_fraud"]),
        num_boost_round=NUM_ROUNDS,
    )
    booster.save_model(str(MODEL_PATH))
    print(f"wrote {MODEL_PATH.relative_to(ROOT)} ({booster.num_trees()} trees)")

    calibrator = IsotonicRegression(out_of_bounds="clip", y_min=0.0, y_max=1.0)
    calibrator.fit(booster.predict(sig.signal_matrix(calibrate)),
                   calibrate["is_fraud"].to_numpy())
    table = calibration_table(calibrator)
    CALIBRATION_PATH.write_text(json.dumps(table, indent=2) + "\n")
    print(f"wrote {CALIBRATION_PATH.relative_to(ROOT)} "
          f"({len(table['x'])} knots)")

    labels = evaluate["is_fraud"].to_numpy()
    raw = booster.predict(sig.signal_matrix(evaluate))
    calibrated = calibrator.predict(raw)
    print(f"\nranking on the held-out split: ROC AUC {roc_auc_score(labels, raw):.4f}"
          f"   PR AUC {average_precision_score(labels, raw):.4f}")
    report_calibration("uncalibrated model output", raw, labels)
    report_calibration("calibrated risk score", calibrated, labels)
    print("\nScope: this is simulated fraud. Per ADR-0001, detection quality "
          "measured here is not evidence about real fraud.")

    scored = evaluate.copy()
    scored["risk_score"] = calibrated
    chosen = pick_fixture_cases(scored)
    write_fixture(chosen)
    rows = write_fixture_history(payments, chosen)
    print(f"\nwrote {FIXTURE_PATH.relative_to(ROOT)} ({len(chosen)} cases)"
          f" and {FIXTURE_HISTORY_PATH.relative_to(ROOT)} ({rows:,} payments)")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data", nargs="+", type=pathlib.Path, default=DEFAULT_DATA)
    main(parser.parse_args().data)
