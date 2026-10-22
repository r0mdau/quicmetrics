.DEFAULT_GOAL := build

GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK_VERSION ?= v1.9.0

fmt:
	go fmt ./...
.PHONY:fmt

vet: fmt
	go vet ./...
.PHONY:vet

lint: fmt
	$(GOLANGCI_LINT) run
.PHONY:lint

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
.PHONY:vuln

tidy:
	go mod tidy
.PHONY:tidy

test:
	go test -race -cover ./...
.PHONY:test

check: fmt vet lint test
.PHONY:check

build: vet
	@echo ">> building amd64 binaries"
	mkdir -p build
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-w -s" -o build/ ./cmd/...
.PHONY:build

docker-build:
	@echo ">> building docker image"
	docker build -t r0mdau/quicmetrics .
.PHONY:docker-build

compose-up:
	@echo ">> starting docker compose suite"
	docker compose up --build -d
.PHONY:compose-up

compose-down:
	docker compose down
.PHONY:compose-down
