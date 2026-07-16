# Multi-stage Go build. The build stage compiles a static binary (overlay.js is
# embedded via go:embed, so the runtime image carries no assets directory and no
# toolchain). The runtime is distroless static — tiny and rootless.

# ── build stage ──────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

# Build a fully static binary (CGO disabled — pure-Go pgx + aws-sdk).
# Pass --build-arg VERSION=<tag> to stamp `octo-doc version`; defaults to "dev".
ARG VERSION=dev
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/octo-doc ./cmd/octo-doc

# ── runtime stage ────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app

ENV PORT=8080 \
    HOST=0.0.0.0

COPY --from=build /out/octo-doc /app/octo-doc
COPY --from=build /src/migrations /app/migrations

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/octo-doc"]
CMD ["serve"]
