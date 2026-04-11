.PHONY: proto build test clean run install setup

BINARY := agentfab
PROTO_DIR := proto
GEN_DIR := gen

proto:
	@mkdir -p $(GEN_DIR)/agentfab/v1
	protoc \
		--proto_path=$(PROTO_DIR) \
		--go_out=$(GEN_DIR) --go_opt=paths=source_relative \
		--go-grpc_out=$(GEN_DIR) --go-grpc_opt=paths=source_relative \
		$(PROTO_DIR)/agentfab/v1/message.proto \
		$(PROTO_DIR)/agentfab/v1/agent.proto \
		$(PROTO_DIR)/agentfab/v1/controlplane.proto

build: proto
	go build -o $(BINARY) ./cmd/agentfab

test: proto
	go test ./...

clean:
	rm -f $(BINARY)

install: build
	@install -m 755 $(BINARY) /usr/local/bin/$(BINARY) 2>/dev/null || \
		(mkdir -p ~/.local/bin && install -m 755 $(BINARY) ~/.local/bin/$(BINARY))
	@echo "Installed agentfab to $$(command -v agentfab 2>/dev/null || echo /usr/local/bin/$(BINARY))"

setup: build
	@./$(BINARY) setup

run: build
	./$(BINARY) run
