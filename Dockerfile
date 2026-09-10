# syntax=docker/dockerfile:1
# Single-stage build for the open-source finops-proxy core.
#
# Two binaries are produced from one stage:
#   /out/proxy      — the circuit-breaker reverse proxy (the product)
#   /out/mockserver — a dev-only mock LLM upstream, so `docker compose up`
#                     gives a fully offline quickstart with no SaaS dependency.
#
# The Go toolchain stays in the image (one stage, simplest possible build). For
# a smaller production image, add a second stage copying /out/proxy onto alpine
# or scratch; the binary is fully static (CGO_ENABLED=0, pure-Go SQLite).

FROM golang:1.22-alpine

WORKDIR /src

# Pull dependencies first so source edits don't bust the layer cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: SQLite is pure-Go (glebarez/sqlite -> modernc.org/sqlite),
# so the proxy is a fully static binary with no libc/cgo requirement.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mockserver ./cmd/mockserver

# config/pricing.json is REQUIRED at runtime (a missing price table is fatal by
# default), so it is baked into the image. Override with a read-only mount when
# you maintain your own price table.
RUN mkdir -p /app/config
COPY config/pricing.json /app/config/pricing.json

WORKDIR /app
EXPOSE 8080 8081

# Default entrypoint is the proxy; the compose file overrides the command for
# the mockserver service. Run with no -db to keep an in-memory store, or mount a
# volume and pass -db /data/finops.db for persistence.
ENTRYPOINT ["/out/proxy"]
CMD ["-addr", ":8080", "-pricing", "/app/config/pricing.json"]
