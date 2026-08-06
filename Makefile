BIN     := claude-launcher
DIST    := dist
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: run build dist test vet clean

## run: start the launcher from source
run:
	go run .

## build: build the binary for this machine
build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN) .

## dist: cross-compile release binaries (single file each, UI embedded)
dist:
	@mkdir -p $(DIST)
	GOOS=darwin  GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-darwin-amd64 .
	GOOS=linux   GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-amd64 .
	GOOS=linux   GOARCH=arm64 go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-linux-arm64 .
	GOOS=windows GOARCH=amd64 go build -ldflags '$(LDFLAGS)' -o $(DIST)/$(BIN)-windows-amd64.exe .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf $(BIN) $(DIST)
