.PHONY: build test vet clean

build:
	go build -o bin/agent-inbox ./cmd/agent-inbox

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -rf bin/
