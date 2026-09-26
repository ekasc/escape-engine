BINARY := escape
GOBIN ?= $(firstword $(subst :, ,$(shell go env GOPATH)))/bin

.PHONY: build test race vet fmt install clean

build:
	go build -o $(BINARY) .

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

install:
	GOBIN="$(GOBIN)" go build -o "$(GOBIN)/$(BINARY)" .

clean:
	rm -f $(BINARY)
