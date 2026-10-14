# syntax=docker/dockerfile:1
#
# Updater image = Go binary + osmium (built from pinned sources) + docker CLI
# and compose plugin. Everything else (osrm-*, tilemaker, photon, nominatim)
# runs as sibling containers started through `docker compose run`.
FROM alpine:3.24.2 AS osmium
ARG PROTOZERO_VERSION=v1.8.2
ARG LIBOSMIUM_VERSION=v2.23.1
ARG OSMIUM_TOOL_VERSION=v1.19.1
RUN apk add --no-cache build-base cmake git boost-dev expat-dev zlib-dev \
      bzip2-dev lz4-dev nlohmann-json
RUN git clone --depth 1 --branch ${PROTOZERO_VERSION} https://github.com/mapbox/protozero /src/protozero \
 && git clone --depth 1 --branch ${LIBOSMIUM_VERSION} https://github.com/osmcode/libosmium /src/libosmium \
 && git clone --depth 1 --branch ${OSMIUM_TOOL_VERSION} https://github.com/osmcode/osmium-tool /src/osmium-tool
RUN cmake -S /src/osmium-tool -B /build \
      -DCMAKE_BUILD_TYPE=Release \
      -DOSMIUM_INCLUDE_DIR=/src/libosmium/include \
      -DPROTOZERO_INCLUDE_DIR=/src/protozero/include \
      -DCMAKE_INSTALL_PREFIX=/opt/osmium \
 && cmake --build /build -j"$(nproc)" \
 && cmake --install /build \
 && strip /opt/osmium/bin/osmium

FROM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/updater ./cmd/updater

FROM alpine:3.24.2
# Runtime libraries of osmium (must match the boost-dev used to build it).
RUN apk add --no-cache ca-certificates tzdata docker-cli docker-cli-compose \
      libstdc++ expat zlib libbz2 lz4-libs boost1.84-program_options
COPY --from=osmium /opt/osmium/bin/osmium /usr/local/bin/osmium
COPY --from=build /out/updater /usr/local/bin/updater
# Runs as root: it needs the docker socket. See README "Security notes".
ENTRYPOINT ["/usr/local/bin/updater"]
CMD ["run"]
