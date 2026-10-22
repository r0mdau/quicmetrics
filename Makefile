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

docker-build: build
	@echo ">> building docker image"
	docker build -t r0mdau/quicmetrics .
.PHONY:docker-build

compose-up: build
	@echo ">> starting docker compose suite"
	docker compose up --build -d
.PHONY:compose-up

compose-down:
	docker compose down
.PHONY:compose-down

test:
	go test -cover ./...
.PHONY:test

verify: fmt test
.PHONY:verify