.PHONY: clean lint test build

export GO111MODULE=on

default: clean lint test build

clean:
	rm -rf dist/ cover.out

test: clean
	go test -v -cover ./...

build: clean
	go build -v -ldflags "-s -w" -trimpath ./cmd/yaegi-dap/

lint:
	golangci-lint run

