.PHONY: build fmt run test

build:
	go build -o task-tracker ./cmd/task-tracker

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

run:
	go run ./cmd/task-tracker serve

test:
	go test ./...
	go vet ./...
