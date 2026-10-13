# gocache-overlay: build, vet and test targets.
.PHONY: build vet test test-v fmt-check lint
BIN ?= gocache-overlay

build:
	go build -o $(BIN) .

vet:
	go vet ./...

fmt-check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

lint: fmt-check vet

# Integration tests run real go builds against throwaway caches.
test:
	go test -count=1 -timeout 1800s ./...

test-v:
	go test -count=1 -timeout 1800s -v ./... $(if $(RUN),-run '$(RUN)')
