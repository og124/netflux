SHELL := /bin/bash
BIN   := bin/netflux
LAB   := examples/lab
SIM   := LEAF1_ADDR=127.0.0.1:57401 LEAF2_ADDR=127.0.0.1:57402 NETFLUX_PASSWORD=admin

.PHONY: build test lint validate demo sim image clean

build:
	go build -o $(BIN) ./cmd/netflux

test:
	go test -race ./...

lint:
	golangci-lint run ./...

validate: build
	$(BIN) validate -dir $(LAB)

# Simulated leaves + controller; status on http://127.0.0.1:8080.
demo: build
	@trap 'kill 0' EXIT; \
	$(BIN) sim leaf1=127.0.0.1:57401 leaf2=127.0.0.1:57402 & \
	sleep 1; \
	$(SIM) $(BIN) run -dir $(LAB) -state ''

sim: build
	$(BIN) sim leaf1=127.0.0.1:57401 leaf2=127.0.0.1:57402

image:
	docker build -t netflux .

clean:
	rm -rf bin .netflux
