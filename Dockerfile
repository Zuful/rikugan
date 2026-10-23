FROM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /rikugan ./cmd/rikugan

# rikugan serve on Docker Compose reads the containers through the Docker
# CLI and its compose plugin, which the distroless image does not have:
#   docker build --target serve-compose -t rikugan:serve-compose .
# It runs as root to reach the Docker socket (see deploy/compose).
FROM docker:29.9.1-cli AS serve-compose

COPY --from=builder /rikugan /usr/local/bin/rikugan

ENTRYPOINT ["/usr/local/bin/rikugan"]
CMD ["serve"]

# The default image: the MCP server, and rikugan serve on Kubernetes.
FROM gcr.io/distroless/static-debian12

COPY --from=builder /rikugan /rikugan

USER nonroot:nonroot

ENTRYPOINT ["/rikugan"]
