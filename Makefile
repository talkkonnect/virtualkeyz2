.PHONY: all build test vet lint vuln fmt ci

all: build

build:
	go build -o virtualkeyz2 ./cmd/virtualkeyz2

test:
	go test -count=1 ./...

test-race:
	go test -count=1 -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt:
	gofmt -w .

ci: fmt vet test lint vuln
	@echo "CI checks passed."
