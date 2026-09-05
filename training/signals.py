"""Signal derivation, training side.

The signals the model reads are stated once, here, in `SIGNAL_SET`. The serving
side states them once too, in `internal/signals`, and the train/serve fixture is
what forces the two statements to agree -- see `testdata/train_serve_fixture.json`.

Every window and every gap is measured against event time (the timestamp carried
on the authorization), never against the clock. Payments are ordered by event
time then transaction id, because whole-second timestamp resolution puts
genuinely distinct payments at the same event time; see ADR-0006.
"""

import numpy as np
import pandas as pd

# The signals the model reads, in the order the model reads them. One place.
SIGNAL_SET = ["amount", "gap_seconds", "velocity_1h", "velocity_24h"]

# Velocity counter windows, in seconds. A payment sitting exactly `window`
# seconds before the authorization is counted: the lower bound is inclusive.
VELOCITY_WINDOWS = {"velocity_1h": 3600, "velocity_24h": 86400}

_COLUMNS = ["cc_num", "trans_num", "unix_time", "amt", "is_fraud"]


def load_payments(paths):
    """Read the checked-in CSVs into one frame, ordered by card then event time.

    -> DataFrame[card, transaction_id, event_time, amount, is_fraud]
    """
    frames = []
    for path in paths:
        frame = pd.read_csv(path, usecols=_COLUMNS, dtype={"cc_num": str})
        frames.append(frame)
    payments = pd.concat(frames, ignore_index=True)
    payments = payments.rename(columns={
        "cc_num": "card",
        "trans_num": "transaction_id",
        "unix_time": "event_time",
        "amt": "amount",
    })
    payments["event_time"] = payments["event_time"].astype(np.int64)
    payments["amount"] = payments["amount"].astype(np.float64)
    payments["is_fraud"] = payments["is_fraud"].astype(np.int8)
    return _in_key_order(payments)


def _in_key_order(payments):
    """Card, then event time, then transaction id. The ordering, everywhere."""
    return payments.sort_values(
        ["card", "event_time", "transaction_id"], kind="mergesort"
    ).reset_index(drop=True)


def derive(payments):
    """Add every signal in SIGNAL_SET to a frame already in key order.

    Each card's signals come only from that card's own history, and only from
    payments strictly before the authorization being scored, so a signal never
    contains information from its own outcome.
    """
    payments = payments.copy()
    payments["amount"] = payments["amount"].astype(np.float64)

    gap = np.full(len(payments), np.nan)
    velocity = {name: np.zeros(len(payments), dtype=np.float64)
                for name in VELOCITY_WINDOWS}

    for start, stop in card_spans(payments["card"].to_numpy()):
        times = payments["event_time"].to_numpy()[start:stop]

        # The gap to the previous payment on this card. The first payment on a
        # card has no previous payment, so its gap stays missing rather than
        # becoming a number the model would read as small.
        gap[start + 1:stop] = np.diff(times).astype(np.float64)

        # Velocity counters. Predecessors are the earlier rows in key order, so
        # counting them is index arithmetic: everything from the first row
        # inside the window up to (but not including) this one.
        index = np.arange(stop - start)
        for name, window in VELOCITY_WINDOWS.items():
            first_in_window = np.searchsorted(times, times - window, side="left")
            velocity[name][start:stop] = (index - first_in_window).astype(np.float64)

    payments["gap_seconds"] = gap
    for name, counts in velocity.items():
        payments[name] = counts
    return payments


def card_spans(cards):
    """-> [(start, stop), ...] row spans, one per card, over a card-ordered array."""
    if len(cards) == 0:
        return []
    boundaries = np.flatnonzero(cards[1:] != cards[:-1]) + 1
    edges = np.concatenate(([0], boundaries, [len(cards)]))
    return list(zip(edges[:-1], edges[1:]))


def signal_matrix(payments):
    """-> the SIGNAL_SET columns, in order, as the model expects them."""
    return payments[SIGNAL_SET]
