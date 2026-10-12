.PHONY: build test check clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/llm-bench ./cmd/llm-bench

test:
	go test ./...

check:
	go vet ./...
	go test -race ./...
	go test -coverprofile=coverage.out ./...

clean:
	rm -f bin/llm-bench coverage.out
