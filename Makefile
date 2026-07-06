run:
	go run ./cmd/api

build:
	CGO_ENABLED=0 go build -o bin/pixledger ./cmd/api

vet:
	go vet ./...

test:
	go test ./... -race -count=1

test-integration:
	DATABASE_URL=postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable go test ./... -race -count=1

token:
	go run ./cmd/tokengen -sub dev -ttl 24h

compose-up:
	docker compose up --build -d

compose-down:
	docker compose down -v

.PHONY: run build vet test test-integration token compose-up compose-down
