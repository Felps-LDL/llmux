IMAGE ?= llmux-gateway
TAG   ?= dev
# O Postgres do docker-compose, para make run; sobrescreva pelo ambiente.
LLMUX_DATABASE_URL ?= postgres://llmux:llmux@localhost:5432/llmux?sslmode=disable

.PHONY: all build test lint run docker-build up down clean

# all é o primeiro alvo para que `make` sem argumento rode o mesmo que o CI.
all: lint test build

build:
	go build -o bin/gateway ./cmd/gateway

test:
	go test -race -coverprofile=coverage.out ./...

lint:
	golangci-lint run

run:
	LLMUX_DATABASE_URL='$(LLMUX_DATABASE_URL)' go run ./cmd/gateway

docker-build:
	docker build -t $(IMAGE):$(TAG) .

# --build porque o gateway é código deste repositório: sem ele, o compose usaria
# a imagem da última vez e ignoraria as mudanças.
up:
	docker compose -f deploy/docker-compose.yml up --build -d

down:
	docker compose -f deploy/docker-compose.yml down

clean:
	rm -rf bin/ coverage.out
