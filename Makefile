BINARY := pi-go

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
	go install .

clean:
	rm -f $(BINARY)
