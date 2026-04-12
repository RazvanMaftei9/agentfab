# syntax=docker/dockerfile:1.7

# Multi-stage build for the agentfab CLI. The same image is used for the
# control plane, node, and conductor workloads in the kind test fabric --
# only the args differ.

FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git make protoc protobuf-dev

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# Pin the protoc plugins to keep generated code reproducible across builds.
ARG PROTOC_GEN_GO_VERSION=v1.34.2
ARG PROTOC_GEN_GO_GRPC_VERSION=v1.5.1
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION} && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}
ENV PATH=/go/bin:${PATH}

COPY . .

RUN make proto

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
        -ldflags="-s -w -X github.com/razvanmaftei/agentfab/internal/version.Version=${VERSION}" \
        -o /out/agentfab ./cmd/agentfab

# ---------------------------------------------------------------------------

FROM alpine:3.20

# ca-certificates: outbound TLS to LLM providers.
# tzdata: stable timestamps in logs.
# openssl: lets the conductor pod regenerate workspace material if needed.
# coreutils: predictable `sleep`, `cp`, `install` for the conductor's exec sessions.
RUN apk add --no-cache ca-certificates tzdata openssl coreutils

# Non-root runtime user. UID/GID 65532 matches the distroless `nonroot` user
# so PodSpecs that pin to that ID work without modification.
RUN addgroup -g 65532 -S agentfab && \
    adduser -u 65532 -S -G agentfab -H -s /sbin/nologin agentfab

COPY --from=builder /out/agentfab /usr/local/bin/agentfab

# Bake the bundled agent definitions into the image at a stable path. The
# kind ConfigMap points `agents_dir` here so pods do not need to ship the
# YAML/markdown files separately.
COPY --from=builder /src/defaults/agents /opt/agentfab/agents

# /etc/agentfab is the working dir; agents.yaml is mounted in via ConfigMap.
# /var/lib/agentfab is the data dir; backed by an emptyDir or PVC at runtime.
RUN install -d -o agentfab -g agentfab -m 0755 /etc/agentfab /var/lib/agentfab && \
    chown -R agentfab:agentfab /opt/agentfab

USER agentfab:agentfab
WORKDIR /etc/agentfab

EXPOSE 50051

ENTRYPOINT ["/usr/local/bin/agentfab"]
