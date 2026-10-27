.PHONY: build build-arm64 test vet clean

LDFLAGS := -s -w
BINARY := dist/notebook-linux-amd64

build:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

build-arm64:
	mkdir -p dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o dist/notebook-linux-arm64 .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf dist
