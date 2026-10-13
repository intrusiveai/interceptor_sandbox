.PHONY: build test test-release-publisher verify fmt vet clean

build:
	mkdir -p bin
	go build -trimpath -o bin/interceptor ./cmd/interceptor

test:
	go test ./...
	python3 -m unittest discover -s scripts/tests
	$(MAKE) test-release-publisher

test-release-publisher:
	cd tools/release-publisher && GOWORK=off GOPROXY=off GOTOOLCHAIN=local go test -race ./...
	cd tools/release-publisher && GOWORK=off GOPROXY=off GOTOOLCHAIN=local go vet ./...
	cd tools/release-publisher && GOWORK=off GOPROXY=off GOTOOLCHAIN=local python3 -m unittest discover -s scripts/tests

fmt:
	gofmt -w $$(find cmd examples internal -name '*.go' -type f)

vet:
	go vet ./...

verify: fmt vet test build

clean:
	rm -rf bin
