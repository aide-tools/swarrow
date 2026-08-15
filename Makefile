.PHONY: build

build:
	mkdir -p bin
	go build -o bin/swarrow ./cmd/swarrow
