# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/trips ./cmd/trips

FROM alpine:3.24.2
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app
COPY --from=build /out/trips /usr/local/bin/trips
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/trips"]
