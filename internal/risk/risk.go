// Package risk turns a vector of signals into a calibrated risk score.
//
// Both artifacts are produced by the training side and loaded once at startup,
// so scoring an authorization is arithmetic and nothing else. The model is
// evaluated in-process by a pure-Go reader rather than behind an inference
// service, because evaluating a trained tree ensemble is smaller than the call
// overhead of asking another process to do it (ADR-0005).
package risk

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// A Scorer holds the model and the calibration stage for the process's lifetime.
type Scorer struct {
	ensemble    *ensemble
	calibration Calibration
}

// Calibration is the held-out stage that turns the model's ranking into a
// probability. A gradient-boosted tree ranks rather than calibrates, and the
// expected-cost arithmetic multiplies costs by this number, so without it the
// arithmetic is meaningless (ADR-0003).
type Calibration struct {
	Kind string    `json:"kind"`
	X    []float64 `json:"x"`
	Y    []float64 `json:"y"`
}

// Load reads the model and the calibration table. A missing or malformed
// artifact fails here, loudly, rather than presenting later as quietly bad
// decisions.
func Load(modelPath, calibrationPath string) (*Scorer, error) {
	ensemble, err := loadEnsemble(modelPath)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(calibrationPath)
	if err != nil {
		return nil, fmt.Errorf("loading calibration %s: %w", calibrationPath, err)
	}
	var calibration Calibration
	if err := json.Unmarshal(raw, &calibration); err != nil {
		return nil, fmt.Errorf("parsing calibration %s: %w", calibrationPath, err)
	}
	if err := calibration.validate(); err != nil {
		return nil, fmt.Errorf("calibration %s: %w", calibrationPath, err)
	}

	return &Scorer{ensemble: ensemble, calibration: calibration}, nil
}

func (c Calibration) validate() error {
	if c.Kind != "isotonic" {
		return fmt.Errorf("unknown kind %q", c.Kind)
	}
	if len(c.X) == 0 || len(c.X) != len(c.Y) {
		return fmt.Errorf("table has %d inputs and %d outputs", len(c.X), len(c.Y))
	}
	for i, y := range c.Y {
		if y < 0 || y > 1 {
			return fmt.Errorf("output %d is %v, which is not a probability", i, y)
		}
		if i > 0 && c.X[i] < c.X[i-1] {
			return fmt.Errorf("inputs are not sorted at index %d", i)
		}
	}
	return nil
}

// FeatureCount is how many signals the model expects, so a caller can check its
// signal set against the artifact instead of discovering the mismatch in the
// scores.
func (s *Scorer) FeatureCount() int { return s.ensemble.featureCount() }

// Score evaluates the model and the calibration stage, returning a probability
// between zero and one.
func (s *Scorer) Score(signals []float64) (float64, error) {
	if got, want := len(signals), s.ensemble.featureCount(); got != want {
		return 0, fmt.Errorf("model wants %d signals, got %d", want, got)
	}
	return s.calibration.apply(s.ensemble.predict(signals)), nil
}

// apply interpolates the isotonic table: piecewise-linear between knots, and
// clipped to the end values outside them, which is what the training side fits.
func (c Calibration) apply(raw float64) float64 {
	switch {
	case raw <= c.X[0]:
		return c.Y[0]
	case raw >= c.X[len(c.X)-1]:
		return c.Y[len(c.Y)-1]
	}
	// The first knot at or above raw; the interval is the one ending there.
	// Same shape as the linear interpolation scipy does on the training side:
	// slope first, then step along it from the lower knot.
	hi := sort.SearchFloat64s(c.X, raw)
	span := c.X[hi] - c.X[hi-1]
	if span <= 0 {
		return c.Y[hi]
	}
	slope := (c.Y[hi] - c.Y[hi-1]) / span
	return c.Y[hi-1] + slope*(raw-c.X[hi-1])
}
