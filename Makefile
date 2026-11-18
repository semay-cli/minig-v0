BINARY := minig
CMD    := ./cmd/minig

.PHONY: build test race vet fmt clean

build:
	go build -trimpath -ldflags="-s -w" -o bin/$(BINARY) $(CMD)

test:
	go test ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -s -w .

clean:
	rm -rf bin