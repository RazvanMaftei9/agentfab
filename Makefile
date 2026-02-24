.PHONY: proto build test clean run

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
		$(PROTO_DIR)/agentfab/v1/agent.proto

build: proto
	go build -o $(BINARY) ./cmd/agentfab

test: proto
	go test ./...

clean:
	rm -f $(BINARY)

run: build
	./$(BINARY) run
