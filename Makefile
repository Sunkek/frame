.PHONY: test test-race lint vet fmt check tidy

# Run tests once (fast feedback)
test:
	go test ./...

# Run tests with race detector, 3 times (matches CI)
test-race:
	go test -race -count=3 -timeout=120s ./...

# Run go vet
vet:
	go vet ./...

# Run staticcheck (install: go install honnef.co/go/tools/cmd/staticcheck@latest)
lint:
	staticcheck ./...

# Format all source files
fmt:
	gofmt -w -s .

# Full pre-commit check: format, vet, race tests
check: fmt vet test-race

# Tidy module (no-op for zero-dependency packages, but good habit)
tidy:
	go mod tidy
