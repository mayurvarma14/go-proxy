BIN_DIR := bin
PROXY_BIN := $(BIN_DIR)/go-proxy
ECHO_BIN := $(BIN_DIR)/echo

GO ?= go
PKG := ./...

.PHONY: build
build: $(PROXY_BIN) $(ECHO_BIN)

$(PROXY_BIN):
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $@ ./cmd/proxy

$(ECHO_BIN):
	@mkdir -p $(BIN_DIR)
	$(GO) build -o $@ ./cmd/echo

.PHONY: run
run: $(PROXY_BIN) certs
	$(PROXY_BIN) --config config/dev.yaml

.PHONY: test
test:
	$(GO) test -race -count=1 $(PKG)

.PHONY: cover
cover:
	$(GO) test -race -count=1 -coverprofile=coverage.out $(PKG)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: bench
bench:
	$(GO) test -run=^$$ -bench=. -benchmem $(PKG)

.PHONY: vet
vet:
	$(GO) vet $(PKG)

.PHONY: fmt
fmt:
	gofmt -w .

.PHONY: lint
lint:
	@command -v golangci-lint >/dev/null || { echo "golangci-lint not installed: https://golangci-lint.run/"; exit 1; }
	golangci-lint run

.PHONY: certs
certs: server.crt server.key

server.crt server.key:
	openssl req -x509 -newkey rsa:2048 -keyout server.key -out server.crt \
		-days 365 -nodes -subj "/CN=localhost" \
		-addext "subjectAltName=DNS:localhost,IP:127.0.0.1"

.PHONY: docker
docker:
	docker build -t go-proxy:latest .

.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out
