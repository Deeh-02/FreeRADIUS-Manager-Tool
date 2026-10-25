.PHONY: build run test test-integration test-e2e test-load tidy deps

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
# A failed start must fail the run: eval of an empty result succeeds, and every test would
# then skip itself and report ok.
test-integration:
	@env=$$(../scripts/test-db.sh start) || exit 1; eval "$$env"; \
	  go test ./internal/handlers -count=1 -p 1 -timeout 30m; r=$$?; \
	  ../scripts/test-db.sh stop; exit $$r

# Everything above plus the whole system: real FreeRADIUS answering logins, the real backend
# binary over HTTP, and the failure tests (PostgreSQL crash, backend kill -9, FreeRADIUS and
# router outages). Needs the freeradius package. About 6 minutes.
test-e2e:
	@env=$$(../scripts/test-stack.sh start) || exit 1; eval "$$env"; \
	  go test ./... -count=1 -p 1 -timeout 60m; r=$$?; \
	  ../scripts/test-stack.sh stop; exit $$r

# Load tests (docs/phase.md A.11-A.15) on the same stack; sizes via LOAD_* variables, see
# internal/handlers/load_test.go.
test-load:
	@env=$$(../scripts/test-stack.sh start) || exit 1; eval "$$env"; \
	  LOAD_TEST=1 go test ./internal/handlers -run Load -count=1 -v -timeout 60m; r=$$?; \
	  ../scripts/test-stack.sh stop; exit $$r

docker-build:
	docker build -t radius-manager-backend .
