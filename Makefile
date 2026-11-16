PKGS := ./... ./examples/mini-agent/...

.PHONY: build test lint
build:
	go build -o bin/ ./cmd/... ./examples/mini-agent/cmd/...

test:
	go test -race -count=1 $(PKGS)

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet $(PKGS)
	GOOS=linux go vet $(PKGS)
