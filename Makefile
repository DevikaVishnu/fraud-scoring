PYTHON ?= .venv/bin/python
DATA ?= data/fraudTrain.csv data/fraudTest.csv

.PHONY: score test train sweep recon venv fmt pipeline-demo pipeline pipeline-seed pipeline-up pipeline-down

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

## --- the heuristic pipeline explored in ADR-0007 -------------------------
## Not the decision path this repo argues for; `score` above is. See the ADR.

## Consume, score and route with no broker at all: an in-process fake stands in
## for Kafka, so this works on a fresh clone with nothing installed.
pipeline-demo:
	go run ./cmd/pipeline -demo

## Start the single-broker Kafka in docker-compose.yml.
pipeline-up:
	docker compose up -d --wait

## Fill the input topic with synthetic authorizations.
pipeline-seed:
	go run ./cmd/pipeline -seed

## Consume from the input topic and route to approved/review. Needs `pipeline-up`.
pipeline:
	go run ./cmd/pipeline

## Stop the broker and discard its data.
pipeline-down:
	docker compose down -v

venv:
	python3 -m venv .venv
	.venv/bin/pip install -r training/requirements.txt

fmt:
	gofmt -w cmd internal scoring
