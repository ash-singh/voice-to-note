.PHONY: run test cover fmt vet tidy

run:
	go run ./cmd/server

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
