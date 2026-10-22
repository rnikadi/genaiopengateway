VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build test vet lint check fmt run docker

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/gateway ./cmd/gateway

test:
	go test -race ./...

vet:
	go vet ./...

lint:
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

check: vet lint test

fmt:
	gofmt -w .

run: build
	./bin/gateway -config config.yaml

docker:
	docker build --build-arg VERSION=$(VERSION) -t genaiopengateway:$(VERSION) .
