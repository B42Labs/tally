# The fixture stack: four public targets and the comments around them.
.PHONY: up down docs test

## these targets need the cluster
## up: create the cluster
up:
	kind create cluster

## down: delete the cluster
down:
	kind delete cluster

## docs: serve | build the site
docs:
	npm run docs

## test: run the test suite
test:
	go test ./...
