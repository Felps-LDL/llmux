IMAGE ?= llmux-gateway
TAG   ?= dev

.PHONY: all build test lint run docker-build clean

# all é o primeiro alvo para que `make` sem argumento rode o mesmo que o CI.
all: lint test build

build:
	go build -o bin/gateway ./cmd/gateway

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run

run:
	go run ./cmd/gateway

docker-build:
	docker build -t $(IMAGE):$(TAG) .

clean:
	rm -rf bin/ coverage.out
