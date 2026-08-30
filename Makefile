SHELL := /bin/sh

BINARY := picomaild
BUILD_TARGET := ./cmd/picomaild

.PHONY: build test fmt

build:
	go build -o $(BINARY) $(BUILD_TARGET)

test:
	go test ./...

fmt:
	gofmt -w $$(find . -name '*.go' -type f)

