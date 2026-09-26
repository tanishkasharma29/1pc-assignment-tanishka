# Local-first: the default target needs nothing but Go. No Docker, no Redis.

BINARY      := bin/server
PKG         := ./...
CONFIG      := deploy/config.yaml
COMPOSE     := docker compose

.DEFAULT_GOAL := help

## help: list the available targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | awk -F': ' '{printf "  %-20s %s\n", $$1, $$2}'

## run: start the server locally (memory cache, no infrastructure required)
run:
	go run ./cmd/server -config $(CONFIG)

## build: compile a static binary into bin/
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server
	@echo "built $(BINARY)"

## test: run the full suite with the race detector
test:
	go test -race -count=1 $(PKG)

## test-short: run the suite without the race detector (faster feedback)
test-short:
	go test -count=1 $(PKG)

## cover: produce and summarise a coverage report
cover:
	go test -race -count=1 -coverprofile=coverage.out -covermode=atomic $(PKG)
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

## bench-proof: run only the tests that prove the single-call guarantee
bench-proof:
	go test -race -count=1 -v -run 'SingleFlight|BeforeResponseIsFlushed|LeaderCancellation|TwoInstances|WithoutTheLock|ClusterBurst' ./internal/cachemw/
## fmt: format all Go source
fmt:
	gofmt -w -s .

## vet: run go vet
vet:
	go vet $(PKG)

## lint: run golangci-lint if it is installed, otherwise fall back to vet
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; running go vet instead"; \
		go vet $(PKG); \
	fi

## tidy: reconcile go.mod and go.sum
tidy:
	go mod tidy

## check: everything CI would run
check: fmt vet test

## prove: start a server, fire 200 concurrent cold requests, assert 1 upstream call
prove: build
	@echo "starting server..."
	@./$(BINARY) -config $(CONFIG) > /tmp/typecode-prove.log 2>&1 & echo $$! > /tmp/typecode-prove.pid
	@for i in $$(seq 1 50); do \
		curl -fsS -o /dev/null http://localhost:8080/healthz 2>/dev/null && break || sleep 0.2; \
	done
	@N=200 ./scripts/prove.sh http://localhost:8080 200; status=$$?; \
		kill $$(cat /tmp/typecode-prove.pid) 2>/dev/null || true; \
		rm -f /tmp/typecode-prove.pid; \
		exit $$status

## docker-build: build the container image
docker-build:
	docker build -t typecode-proxy:latest .

## docker-up: start the app and Redis (2 containers) on port 8080
docker-up:
	$(COMPOSE) up --build -d
	@echo "waiting for the app (the image runs the test suite on build)..."
	@for i in $$(seq 1 120); do \
		curl -fsS -o /dev/null http://localhost:8080/healthz 2>/dev/null && break || sleep 1; \
	done
	@echo ""
	@echo "  app   : http://localhost:8080"
	@echo "  redis : internal to the compose network"
	@echo ""
	@echo "  next: make docker-prove"

## docker-prove: run the concurrency proof against the container
docker-prove:
	@./scripts/prove.sh http://localhost:8080 200

## docker-ps: show container status
docker-ps:
	$(COMPOSE) ps

## docker-logs: follow the application logs
docker-logs:
	$(COMPOSE) logs -f app

## docker-redis: open a redis-cli session inside the redis container
docker-redis:
	$(COMPOSE) exec redis redis-cli

## docker-down: tear everything down, including volumes
docker-down:
	$(COMPOSE) down -v --remove-orphans

## docker-up-cluster: optional 4-container setup (2 replicas + Redis + nginx on 8000)
docker-up-cluster:
	$(COMPOSE) -f docker-compose.yml -f docker-compose.cluster.yml up --build -d
	@echo "waiting for the load balancer..."
	@for i in $$(seq 1 120); do \
		curl -fsS -o /dev/null http://localhost:8000/healthz 2>/dev/null && break || sleep 1; \
	done
	@echo ""
	@echo "  load balancer : http://localhost:8000"
	@echo "  replica app   : http://localhost:8080"
	@echo "  replica app-2 : http://localhost:8082"

## prove-cluster: proof across the cluster, summing upstream calls from both replicas
 prove-cluster:
	@./scripts/prove.sh http://localhost:8000 200 "http://localhost:8080 http://localhost:8082"

## docker-down-cluster: tear down the 4-container setup
docker-down-cluster:
	$(COMPOSE) -f docker-compose.yml -f docker-compose.cluster.yml down -v --remove-orphans

## clean: remove build artefacts
clean:
	rm -rf bin coverage.out coverage.html

.PHONY: help run build test test-short cover bench-proof fmt vet lint tidy check \
        prove docker-build docker-up docker-prove docker-ps docker-logs docker-redis \
        docker-down docker-up-cluster prove-cluster docker-down-cluster clean
