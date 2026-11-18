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
	GOOS=darwin go vet $(PKGS)
	@# Only Linux and macOS are supported. Elsewhere every package outside
	@# internal/ must fail to build, and only at the files that say so
	@# (unsupported.go), never somewhere deeper.
	@for os in windows freebsd; do \
		out=$$(GOOS=$$os go build $(PKGS) 2>&1); \
		if echo "$$out" | grep -v '^#' | grep -qv 'undefined: subactl_supports_only_linux_and_macos'; then \
			echo "GOOS=$$os: the build must stop only at unsupported.go:"; echo "$$out"; exit 1; \
		fi; \
		for p in $$(go list $(PKGS) | grep -v /internal/); do \
			if GOOS=$$os go build $$p >/dev/null 2>&1; then echo "GOOS=$$os: $$p builds; it needs an unsupported.go"; exit 1; fi; \
		done; \
	done

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
