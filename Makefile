.PHONY: run test cover fmt vet tidy loadtest

run:
	go run ./cmd/server

# Load-tests the API/queue/worker path against a local mock LLM/sink, never a
# real provider. Uses dedicated ports (18080/19090) and a scratch QUEUE_DIR so
# it never touches a real dev server or its queue. See docs/load_test_summary.md.
# Override with e.g. `make loadtest QUEUE_WORKERS=8 N=500 C=50`.
QUEUE_WORKERS ?= 2
N ?= 200
C ?= 20
loadtest:
	go build -o /tmp/vtn-server ./cmd/server
	go build -o /tmp/mockapi ./tools/loadtest/mockapi
	go build -o /tmp/loadgen ./tools/loadtest/gen
	@if lsof -tiTCP:18080 -sTCP:LISTEN >/dev/null 2>&1 || lsof -tiTCP:19090 -sTCP:LISTEN >/dev/null 2>&1; then \
		echo "port 18080 or 19090 already in use, aborting"; exit 1; \
	fi
	mkdir -p /tmp/loadtest-queue
	/tmp/mockapi -addr :19090 & echo $$! > /tmp/loadtest-mockapi.pid
	sleep 1
	LLM_API_KEY=test-key LLM_BASE_URL=http://127.0.0.1:19090/v1 \
		SINK=webhook WEBHOOK_URL=http://127.0.0.1:19090/webhook \
		ADDR=:18080 QUEUE_DIR=/tmp/loadtest-queue QUEUE_WORKERS=$(QUEUE_WORKERS) \
		QUEUE_MAX_DEPTH=100 LOG_LEVEL=warn \
		/tmp/vtn-server & echo $$! > /tmp/loadtest-server.pid
	sleep 1
	/tmp/loadgen -addr http://127.0.0.1:18080 -n $(N) -c $(C) -label "w$(QUEUE_WORKERS)-c$(C)-n$(N)"
	@kill $$(cat /tmp/loadtest-server.pid) $$(cat /tmp/loadtest-mockapi.pid) 2>/dev/null; \
		rm -f /tmp/loadtest-server.pid /tmp/loadtest-mockapi.pid; \
		rm -rf /tmp/loadtest-queue

test:
	go test -race ./...

cover:
	go test -coverprofile=coverage.out ./... && go tool cover -func=coverage.out | tail -1

fmt:
	gofmt -l -w .

vet:
	go vet ./...

tidy:
	go mod tidy
