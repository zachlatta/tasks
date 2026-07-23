.PHONY: build fmt run test

build:
	go build -o tasks ./cmd/tasks

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

run:
	go run ./cmd/tasks serve

test:
	go test ./...
	go vet ./...
