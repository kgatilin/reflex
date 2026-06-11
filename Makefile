# reflex — single-binary build. `make install` puts `reflex` on your PATH
# (GOBIN, falling back to GOPATH/bin).

.PHONY: build test vet install

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

install:
	go install ./cmd/reflex
