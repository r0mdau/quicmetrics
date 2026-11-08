.DEFAULT_GOAL := build

TOOL := go tool -modfile=tools/go.mod
PLATFORMS ?= linux/amd64 linux/arm64
FUZZTIME ?= 30s

fmt:
	go fmt ./...
.PHONY: fmt

vet:
	go vet ./...
.PHONY: vet

lint:
	$(TOOL) golangci-lint run
.PHONY: lint

vuln:
	$(TOOL) govulncheck ./...
.PHONY: vuln

tidy:
	go mod tidy
	cd tools && go mod tidy
.PHONY: tidy

test:
	go test -race -cover ./...
.PHONY: test

bench:
	go test -run '^$$' -bench . -benchmem ./...
.PHONY: bench

fuzz:
	@for pkg in $$(go list ./...); do \
		for target in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz'); do \
			echo ">> fuzzing $$pkg $$target for $(FUZZTIME)"; \
			go test -run '^$$' -fuzz "^$$target\$$" -fuzztime $(FUZZTIME) $$pkg || exit 1; \
		done; \
	done
.PHONY: fuzz

check: vet lint test
.PHONY: check

build: vet
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo ">> building $$os/$$arch binaries in build/$${os}_$$arch"; \
		mkdir -p build/$${os}_$$arch && \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-w -s" -o build/$${os}_$$arch/ ./cmd/... || exit 1; \
	done
.PHONY: build

docker-build:
	@echo ">> building docker image"
	docker build -t r0mdau/quicmetrics .
.PHONY: docker-build

compose-up:
	@echo ">> starting docker compose suite"
	docker compose up --build -d
.PHONY: compose-up

compose-down:
	docker compose down
.PHONY: compose-down
