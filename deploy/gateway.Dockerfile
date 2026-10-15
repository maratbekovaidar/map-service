# syntax=docker/dockerfile:1
FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.24.2
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app \
 && mkdir -p /state && chown app:app /state
COPY --from=build /out/gateway /usr/local/bin/gateway
# Style, glyphs and sprites are served as static files by the gateway.
COPY deploy/tiles/styles /app/static/styles
COPY deploy/tiles/preview.html /app/static/preview.html
COPY deploy/tiles/fonts /app/static/fonts
COPY deploy/tiles/sprites /app/static/sprites
COPY deploy/tiles/vendor /app/static/vendor
USER app
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/gateway"]
