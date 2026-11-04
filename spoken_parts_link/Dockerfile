# Spoken Parts Link image. Build from the repository root, for example:
#   docker buildx build -f spoken_parts_link/Dockerfile \
#     --platform linux/amd64,linux/arm64 --build-arg VERSION=0.1.1 .
#
# build.yaml is deprecated (2026-04), so the base image, arguments and labels
# all live here. The builder image is pinned by digest.

ARG GO_IMAGE=golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.1.1
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOFLAGS=-buildvcs=false \
    go build -trimpath -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
    -o /out/spoken-parts-link ./cmd/spoken-parts-link

# Home Assistant names the arm64 architecture "aarch64".
FROM scratch AS base-amd64
LABEL io.hass.arch="amd64"
FROM scratch AS base-arm64
LABEL io.hass.arch="aarch64"

FROM base-${TARGETARCH}
ARG VERSION=0.1.1
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/spoken-parts-link /usr/bin/spoken-parts-link
LABEL io.hass.type="addon" \
      io.hass.name="Spoken Parts Link" \
      io.hass.description="Shows the status of your Bambu Lab printers in Spoken Parts." \
      io.hass.version="${VERSION}" \
      org.opencontainers.image.title="Spoken Parts Link" \
      org.opencontainers.image.description="Shows the status of your Bambu Lab printers in Spoken Parts." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.vendor="ArrowTech Labs" \
      org.opencontainers.image.url="https://spokenparts.com" \
      org.opencontainers.image.source="https://github.com/ArrowTech-Labs/spoken-parts-link" \
      org.opencontainers.image.licenses="PolyForm-Shield-1.0.0"
# Starts as root only to fix /data ownership, then drops to uid 65532.
ENTRYPOINT ["/usr/bin/spoken-parts-link"]
