# syntax=docker/dockerfile:1
# Build natively on the build host and cross-compile for the target, so
# `--platform linux/amd64` (Unraid) is fast from any machine.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.3-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/gpodder-sync . \
 && mkdir /out/data

# Distroless: no shell, no package manager.
FROM gcr.io/distroless/static-debian13
ARG VERSION=dev
LABEL org.opencontainers.image.title="gpodder-sync" \
      org.opencontainers.image.description="gpodder.net-compatible podcast sync server for AntennaPod (auth via reverse proxy)" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/gpodder-sync /gpodder-sync
# Unraid convention: run as nobody:users (99:100), which owns /mnt/user/appdata.
COPY --from=build --chown=99:100 /out/data /data
USER 99:100
ENV GPODDER_SYNC_LISTEN=:8080 \
    GPODDER_SYNC_DB=/data/gpodder-sync.db
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s CMD ["/gpodder-sync", "healthcheck"]
ENTRYPOINT ["/gpodder-sync"]
CMD ["serve"]
