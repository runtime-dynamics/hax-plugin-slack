BINARY := hax-plugin-slack
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X main.version=$(VERSION) \
	-X main.commit=$(COMMIT) \
	-X main.date=$(DATE)

.PHONY: build clean test lint setup-hooks

build:
	go build -ldflags '$(LDFLAGS)' -o $(BINARY) .

clean:
	rm -f $(BINARY)

test:
	go test -v -race ./...

lint:
	golangci-lint run

setup-hooks:
	git config core.hooksPath .githooks

install: build
	cp $(BINARY) $(GOPATH)/bin/$(BINARY)
