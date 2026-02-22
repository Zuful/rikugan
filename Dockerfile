FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /rikugan ./cmd/rikugan

FROM gcr.io/distroless/static-debian12

COPY --from=builder /rikugan /rikugan

USER nonroot:nonroot

ENTRYPOINT ["/rikugan"]
