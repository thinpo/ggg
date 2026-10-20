BINARY := ggg
# The trailing $$ is needed: make would eat a single $ in the sed pattern.
VERSION ?= $(shell sed -n 's/^var version = "\(.*\)"$$/\1/p' main.go)
LDFLAGS := -s -w -X main.version=$(VERSION)
DIST := dist

.PHONY: all build test fmt vet install release clean

all: fmt vet test build

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

# Build for the local platform into ~/local/bin.
install:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(HOME)/local/bin/$(BINARY) .

test:
	./test.sh "$$PWD/$(BINARY)"

fmt:
	gofmt -l -w .

vet:
	go vet ./...

# Cross-compile every release binary, tag and publish to GitHub.
release:
	./release.sh $(VERSION)

clean:
	rm -rf $(BINARY) $(DIST)
