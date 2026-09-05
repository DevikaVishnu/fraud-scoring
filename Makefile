PYTHON ?= .venv/bin/python
DATA ?= data/fraudTrain.csv data/fraudTest.csv

.PHONY: score test train sweep recon venv fmt

## Score one authorization end to end and print the decision. Needs nothing but
## a clone: it runs against the checked-in history slice and artifacts.
score:
	go run ./cmd/score

## The whole Go suite, including the train/serve consistency fixture.
test:
	go test ./...

## Retrain from the payment files and re-export every artifact, the calibration
## table and the train/serve fixture. Needs the full data; see the README.
train:
	$(PYTHON) training/train.py --data $(DATA)

## How far the decision mix moves across plausible cost inputs (ADR-0003).
sweep:
	$(PYTHON) training/sweep.py --data $(DATA)

## The two reconnaissance runs behind ADR-0002 and ADR-0006. Standard library only.
recon:
	python3 scripts/gap_recon.py $(DATA)
	python3 scripts/rule_eval.py $(DATA)

venv:
	python3 -m venv .venv
	.venv/bin/pip install -r training/requirements.txt

fmt:
	gofmt -w cmd internal scoring
