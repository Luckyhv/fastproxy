# ─── build stage ──────────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go.mod first so `go mod download` is cached unless deps change.
# (No go.sum yet because we have zero external dependencies — pure stdlib.)
COPY go.mod ./
RUN go mod download

COPY . .

# Static, stripped binary: CGO off so it runs on a bare scratch image, and
# -w -s drops debug info to shrink it.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o fastproxy .

# ─── runtime stage ──────────────────────────────────────────────────────────────
# scratch = empty image. The only thing a static Go binary needs from outside is
# CA certificates (to verify upstream HTTPS), which we copy from the builder.
FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /app/fastproxy /fastproxy

ENV PORT=3847
EXPOSE 3847

ENTRYPOINT ["/fastproxy"]
