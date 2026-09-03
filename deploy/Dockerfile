# ──────────────────────────────────────────────────────────────────────────────
# Multi-stage build: Go builder -> distroless static runtime
# ──────────────────────────────────────────────────────────────────────────────

FROM golang:1.26-alpine AS builder

WORKDIR /app

# Cache dependencies first (only re-downloads when go.mod/go.sum change).
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build a static, stripped binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/query-guard ./cmd/queryguard

# ──────────────────────────────────────────────────────────────────────────────
# Runtime image
# ──────────────────────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# Prefer the config mounted by the Helm chart; distroless has no shell, so the
# path is fixed via a flag.
COPY --from=builder /out/query-guard /query-guard
COPY policy.yaml /etc/query-guard/policy.yaml

EXPOSE 8090

USER nonroot:nonroot

ENTRYPOINT ["/query-guard"]
CMD ["-config", "/etc/query-guard/policy.yaml"]