package risk

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// A trained gradient-boosted tree is a lump of thresholds, and evaluating it is
// arithmetic. This is that arithmetic, reading LightGBM's own text format, so
// nothing here wraps a C++ runtime or pulls a heavier dependency into the
// container than the workload deserves (ADR-0005).

// An ensemble is a model file: its feature names, and its trees.
type ensemble struct {
	featureNames []string
	sigmoid      float64
	trees        []tree
}

// A tree is stored the way LightGBM stores it: parallel arrays indexed by
// internal node, with children encoded as an index into the same arrays when
// non-negative and as ^leaf when negative.
type tree struct {
	splitFeature []int
	threshold    []float64
	decisionType []int
	leftChild    []int
	rightChild   []int
	leafValue    []float64
}

// LightGBM's flags on a numerical split, and its notion of zero.
const (
	flagCategorical  = 1
	flagDefaultLeft  = 2
	missingTypeShift = 2
	missingTypeNone  = 0
	missingTypeZero  = 1
	missingTypeNaN   = 2
	zeroThreshold    = 1e-35
)

func loadEnsemble(path string) (*ensemble, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("loading model %s: %w", path, err)
	}
	defer file.Close()

	loaded := &ensemble{sigmoid: 1}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var current *tree
	for line := 1; scanner.Scan(); line++ {
		key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
		if !ok {
			if strings.TrimSpace(scanner.Text()) == "end of trees" {
				break
			}
			continue
		}
		if err := loaded.absorb(&current, key, value); err != nil {
			return nil, fmt.Errorf("loading model %s line %d: %w", path, line, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("loading model %s: %w", path, err)
	}
	return loaded, loaded.validate(path)
}

func (e *ensemble) absorb(current **tree, key, value string) error {
	if key == "Tree" {
		e.trees = append(e.trees, tree{})
		*current = &e.trees[len(e.trees)-1]
		return nil
	}

	if *current == nil {
		switch key {
		case "feature_names":
			e.featureNames = strings.Fields(value)
		case "objective":
			return e.absorbObjective(value)
		case "num_class", "num_tree_per_iteration":
			if value != "1" {
				return fmt.Errorf("%s=%s: only single-output models are read here", key, value)
			}
		}
		return nil
	}

	var err error
	switch key {
	case "num_cat":
		if value != "0" {
			return fmt.Errorf("num_cat=%s: categorical splits are not read here", value)
		}
	case "split_feature":
		(*current).splitFeature, err = parseInts(value)
	case "decision_type":
		(*current).decisionType, err = parseInts(value)
	case "left_child":
		(*current).leftChild, err = parseInts(value)
	case "right_child":
		(*current).rightChild, err = parseInts(value)
	case "threshold":
		(*current).threshold, err = parseFloats(value)
	case "leaf_value":
		(*current).leafValue, err = parseFloats(value)
	}
	return err
}

// absorbObjective reads e.g. "binary sigmoid:1". Only the binary objective is
// read: it is the one this project trains, and a wrong transformation would
// silently produce a number that is not a probability.
func (e *ensemble) absorbObjective(value string) error {
	fields := strings.Fields(value)
	if len(fields) == 0 || fields[0] != "binary" {
		return fmt.Errorf("objective=%s: only the binary objective is read here", value)
	}
	for _, field := range fields[1:] {
		if parameter, argument, ok := strings.Cut(field, ":"); ok && parameter == "sigmoid" {
			sigmoid, err := strconv.ParseFloat(argument, 64)
			if err != nil {
				return fmt.Errorf("objective=%s: %w", value, err)
			}
			e.sigmoid = sigmoid
		}
	}
	return nil
}

func (e *ensemble) validate(path string) error {
	if len(e.trees) == 0 {
		return fmt.Errorf("model %s has no trees", path)
	}
	for i, t := range e.trees {
		nodes := len(t.splitFeature)
		if nodes == 0 || len(t.threshold) != nodes || len(t.decisionType) != nodes ||
			len(t.leftChild) != nodes || len(t.rightChild) != nodes {
			return fmt.Errorf("model %s tree %d: inconsistent node arrays", path, i)
		}
		if len(t.leafValue) != nodes+1 {
			return fmt.Errorf("model %s tree %d: %d nodes but %d leaves",
				path, i, nodes, len(t.leafValue))
		}
	}
	return nil
}

func (e *ensemble) featureCount() int { return len(e.featureNames) }

// predict evaluates every tree and squashes the sum, matching what LightGBM's
// own predict_proba returns for a binary objective.
func (e *ensemble) predict(features []float64) float64 {
	raw := 0.0
	for i := range e.trees {
		raw += e.trees[i].leaf(features)
	}
	return 1 / (1 + math.Exp(-e.sigmoid*raw))
}

// leaf walks one tree, following LightGBM's own rule at each split: a missing
// value goes whichever way the split says missing values go, and everything
// else compares against the threshold.
func (t *tree) leaf(features []float64) float64 {
	node := 0
	for {
		value := features[t.splitFeature[node]]
		flags := t.decisionType[node]
		missing := (flags >> missingTypeShift) & 3

		if math.IsNaN(value) && missing != missingTypeNaN {
			value = 0
		}
		var child int
		switch {
		case missing == missingTypeNaN && math.IsNaN(value),
			missing == missingTypeZero && isZero(value):
			if flags&flagDefaultLeft != 0 {
				child = t.leftChild[node]
			} else {
				child = t.rightChild[node]
			}
		case value <= t.threshold[node]:
			child = t.leftChild[node]
		default:
			child = t.rightChild[node]
		}

		if child < 0 {
			return t.leafValue[^child]
		}
		node = child
	}
}

func isZero(value float64) bool {
	return value > -zeroThreshold && value <= zeroThreshold
}

func parseInts(value string) ([]int, error) {
	fields := strings.Fields(value)
	parsed := make([]int, len(fields))
	for i, field := range fields {
		number, err := strconv.Atoi(field)
		if err != nil {
			return nil, err
		}
		parsed[i] = number
	}
	return parsed, nil
}

func parseFloats(value string) ([]float64, error) {
	fields := strings.Fields(value)
	parsed := make([]float64, len(fields))
	for i, field := range fields {
		number, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return nil, err
		}
		parsed[i] = number
	}
	return parsed, nil
}
