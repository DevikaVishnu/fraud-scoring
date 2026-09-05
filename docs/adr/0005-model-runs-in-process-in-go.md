---
Status: accepted
---

# The model runs in-process in Go; there is no inference service

Python trains the model and never sees a live authorization. Training exports a LightGBM model file plus a small calibration table, and the Go scoring service loads both and evaluates them in-process.

The expected shape here is a Python inference service behind gRPC, so its absence needs explaining. A gradient-boosted tree is a lump of thresholds once trained, and evaluating it is arithmetic — a millisecond or two. Standing a separate service in front of that adds a network hop, a serialization step, a process to keep alive and a failure mode to handle, all to wrap a computation smaller than the call overhead. Published breakdowns of comparable systems put model inference at 1-2ms against a ~100ms budget; the budget goes on fetching signals, not on scoring.

Use a pure-Go model reader rather than ONNX, whose Go bindings wrap a C++ runtime and pull a heavier dependency into the container than this needs. The calibration stage ships alongside: isotonic regression as a lookup table, or Platt scaling as two numbers.