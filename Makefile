.PHONY: build test vet smoke clean

build:
	go build -o bin/agent-inbox ./cmd/agent-inbox

test:
	go test ./...

vet:
	go vet ./...

smoke: build
	bash scripts/smoke.sh

clean:
	rm -rf bin/
