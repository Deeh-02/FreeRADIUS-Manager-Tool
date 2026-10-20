.PHONY: build run test test-integration tidy deps

build:
	go build -o ./bin/radius-manager ./main.go

run:
	go run ./main.go

deps:
	go mod download

tidy:
	go mod tidy

# Unit tests; the database integration tests skip themselves unless TEST_DATABASE_URL is set.
test:
	go test ./... -v

# Billing integration tests (concurrency, edge cases, time simulation) against a scratch
# PostgreSQL: the whole thing in one command. For the long simulation see sim_test.go.
test-integration:
	@eval "$$(../scripts/test-db.sh start)"; \
	  go test ./internal/handlers -count=1 -p 1 -timeout 30m; r=$$?; \
	  ../scripts/test-db.sh stop; exit $$r

docker-build:
	docker build -t radius-manager-backend .
