PKGS := ./... ./examples/mini-agent/...

.PHONY: build test lint integration
build:
	go build -o bin/ ./cmd/... ./examples/mini-agent/cmd/...

test:
	go test -race -count=1 $(PKGS)

lint:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet $(PKGS)
	GOOS=linux go vet $(PKGS)

# Integration tests need root and cgroup v2. On Linux they run directly
# (sudo); elsewhere, in a privileged Docker container.
integration:
	GOOS=linux go test -c -tags integration -o bin-linux/daemon.test ./internal/daemon
	@if [ "$$(uname)" = Linux ]; then \
		sudo SUBACTL_TEST_UID=$$(id -u) bin-linux/daemon.test -test.run Integration -test.count=1 -test.v; \
	else \
		docker run --rm --privileged -v "$$PWD/bin-linux:/opt/subactl:ro" node:18-bookworm \
			/opt/subactl/daemon.test -test.run Integration -test.count=1 -test.v; \
	fi
