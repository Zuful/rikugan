# Build from the repository root:
#   docker build -f test/e2e/app/Dockerfile -t rikugan-e2e-app:test .
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY test/e2e/app ./test/e2e/app
RUN CGO_ENABLED=0 go build -o /app ./test/e2e/app

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /app /app
USER nonroot
ENTRYPOINT ["/app"]
