.DEFAULT_GOAL := build

fmt:
	go fmt ./...
.PHONY:fmt

lint: fmt
	golint ./...
.PHONY:lint

vet: fmt
	go vet ./...
.PHONY:vet

build: vet
	@echo ">> building amd64 binary"
	mkdir -p build
	GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o build/ -v ./...
.PHONY:build

test:
	go test -cover ./...
.PHONY:test

verify: fmt test
.PHONY:verify