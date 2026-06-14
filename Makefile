.PHONY: all build test bench clean deps vet

BINARY_NAME=wirebonding-gateway
CMD_PATH=./cmd/gateway
CGO_ENABLED=1
GO_BUILD=CGO_ENABLED=$(CGO_ENABLED) go build
GO_TEST=CGO_ENABLED=$(CGO_ENABLED) go test

all: deps vet build

deps:
	go mod download
	go mod verify

build:
	$(GO_BUILD) -ldflags "-s -w" -o $(BINARY_NAME) $(CMD_PATH)

test:
	$(GO_TEST) -v -race -count=1 ./internal/... ./pkg/...

bench:
	$(GO_TEST) -bench=. -benchmem -run=^$$ -benchtime=3s ./internal/dsp/... ./internal/iec61850/...

vet:
	go vet ./...

clean:
	rm -f $(BINARY_NAME)
	go clean -testcache

pprof-cpu:
	$(GO_TEST) -cpuprofile=cpu.prof -bench=BenchmarkSlidingDFT ./internal/dsp/...
	go tool pprof -top cpu.prof

pprof-mem:
	$(GO_TEST) -memprofile=mem.prof -bench=BenchmarkSlidingDFT ./internal/dsp/...
	go tool pprof -top mem.prof
