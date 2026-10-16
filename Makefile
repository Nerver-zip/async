BINARY := async
VERSION ?= 0.1.0
LDFLAGS := -s -w -X main.Version=$(VERSION)

.PHONY: all build test vet fmt check-cross install clean

all: build

build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/async

test:
	go test -v -count=1 -race ./...
	python3 -m unittest discover -s .github/scripts -p 'test_*.py'

vet:
	go vet ./...

fmt:
	gofmt -s -w .

check-cross:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /dev/null ./cmd/async
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /dev/null ./cmd/async
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o /dev/null ./cmd/async
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o /dev/null ./cmd/async

install: build
	install -Dm755 bin/$(BINARY) ~/.local/bin/$(BINARY)

clean:
	rm -rf bin/ dist/
