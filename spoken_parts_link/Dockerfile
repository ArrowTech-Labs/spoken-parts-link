# Spoken Parts Link image. Build from the repository root, for example:
#   docker buildx build -f spoken_parts_link/Dockerfile \
#     --platform linux/amd64,linux/arm64 --build-arg VERSION=0.3.1 .
#
# build.yaml is deprecated (2026-04), so the base image, arguments and labels
# all live here. The builder image is pinned by digest.

ARG GO_IMAGE=golang:1.27-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb
# 0.3: ffmpeg for camera stills comes from Alpine (alpine:3.22, pinned).
ARG ALPINE_IMAGE=alpine:3.22@sha256:49340b346798875b5592e5d420241f2f9cfba5f47e62ebb1446a1b34734e898f
ARG FFMPEG_VERSION=6.1.2-r2

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=0.3.1
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOFLAGS=-buildvcs=false \
    go build -trimpath -ldflags "-s -w -buildid= -X main.version=${VERSION}" \
    -o /out/spoken-parts-link ./cmd/spoken-parts-link

# ffmpeg and its libraries for the target architecture, installed into a
# separate root by the build platform's apk (signed packages, no scripts), so
# no emulation is needed. The link only ever runs it with the H.264 key frame
# on stdin and the JPEG on stdout: no URL, no access code (internal/camera).
FROM --platform=$BUILDPLATFORM ${ALPINE_IMAGE} AS ffmpeg
ARG TARGETARCH
ARG FFMPEG_VERSION
RUN case "${TARGETARCH}" in amd64) A=x86_64 ;; arm64) A=aarch64 ;; *) exit 1 ;; esac && \
    apk add --root /rootfs --initdb --arch "$A" --keys-dir "/usr/share/apk/keys/$A" \
      --repositories-file /etc/apk/repositories --no-cache --no-scripts "ffmpeg=${FFMPEG_VERSION}" && \
    rm -rf /rootfs/usr/share /rootfs/usr/include /rootfs/var /rootfs/lib/apk

# Home Assistant names the arm64 architecture "aarch64".
FROM scratch AS base-amd64
LABEL io.hass.arch="amd64"
FROM scratch AS base-arm64
LABEL io.hass.arch="aarch64"

FROM base-${TARGETARCH}
ARG VERSION=0.3.1
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=ffmpeg /rootfs/lib/ /lib/
COPY --from=ffmpeg /rootfs/usr/lib/ /usr/lib/
COPY --from=ffmpeg /rootfs/usr/bin/ffmpeg /usr/bin/ffmpeg
COPY --from=build /out/spoken-parts-link /usr/bin/spoken-parts-link
LABEL io.hass.type="addon" \
      io.hass.name="Spoken Parts Link" \
      io.hass.description="Sends prints from Spoken Parts to your Bambu Lab printers, shows their status and watches them for problems." \
      io.hass.version="${VERSION}" \
      org.opencontainers.image.title="Spoken Parts Link" \
      org.opencontainers.image.description="Sends prints from Spoken Parts to your Bambu Lab printers, shows their status and watches them for problems." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.vendor="ArrowTech Labs" \
      org.opencontainers.image.url="https://spokenparts.com" \
      org.opencontainers.image.source="https://github.com/ArrowTech-Labs/spoken-parts-link" \
      org.opencontainers.image.licenses="PolyForm-Shield-1.0.0"
# Starts as root only to fix /data ownership, then drops to uid 65532.
ENTRYPOINT ["/usr/bin/spoken-parts-link"]
