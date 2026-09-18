ARG GO_VERSION=1.27
ARG VENDOR="machinectl"

# Stage 1: Build the Go manager and shim
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
WORKDIR /app
COPY go.mod go.sum ./
COPY hack/litefs ./hack/litefs
# Upstream LiteFS at the pinned tag plus our patches; go.mod replaces the
# module with this directory.
RUN hack/litefs/fetch.sh
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim

# Stage 2: Extract Plex and set up the filesystem
FROM --platform=${BUILDPLATFORM} ubuntu:latest AS extractor
ARG TARGETARCH
ARG VENDOR
ARG VERSION

WORKDIR /plex-build

RUN \
  echo "**** install dependencies ****" && \
  apt-get update && apt-get install -y wget xz-utils ca-certificates jq && \
  echo "**** install plex ****" && \
  if [ -z ${VERSION+x} ]; then \
    VERSION=$(wget -qO - 'https://plex.tv/api/downloads/5.json' \
    | jq -r '.computer.Linux.version'); \
  fi && \
  wget "https://downloads.plex.tv/plex-media-server-new/${VERSION}/debian/plexmediaserver_${VERSION}_${TARGETARCH}.deb" -O \
    plex.deb && \
  dpkg-deb -x plex.deb rootfs && \
    rm plex.deb

# Hijack binaries and setup symlinks
RUN cd rootfs/usr/lib/plexmediaserver && \
    mv "Plex Transcoder" "Plex Transcoder.real" && \
    mv "Plex Media Scanner" "Plex Media Scanner.real" && \
    mv "Plex Commercial Skipper" "Plex Commercial Skipper.real" && \
    mv "Plex Relay" "Plex Relay.real"

# The shims will point to /usr/local/bin/shim in the final image
# (We do this here because distroless has no 'ln' command)
RUN cd rootfs/usr/lib/plexmediaserver && \
    ln -s /usr/local/bin/shim "Plex Transcoder" && \
    ln -s /usr/local/bin/shim "Plex Media Scanner" && \
    ln -s /usr/local/bin/shim "Plex Commercial Skipper" && \
    ln -s /usr/local/bin/shim "Plex Relay"

# Prepare empty state directories needed by Plex and LiteFS
RUN mkdir -p  rootfs/var/lib/litefs rootfs/var/lib/plexmediaserver

# Stage 3: Final image
FROM --platform=${BUILDPLATFORM} debian:bookworm-slim
# fuse3 for LiteFS; iptables for the port redirect in front of Plex (ADR 0002).
RUN apt-get update && apt-get install -y fuse3 iptables ca-certificates && rm -rf /var/lib/apt/lists/*

ARG VENDOR

# Copy LiteFS and Custom Binaries
COPY --from=builder /app/bin/manager /usr/local/bin/manager
COPY --from=builder /app/bin/shim /usr/local/bin/shim

# Copy the extracted Plex root filesystem over
COPY --from=extractor /plex-build/rootfs /

# Set environment variables commonly required by Plex
ENV DEBIAN_FRONTEND="noninteractive" \
    NVIDIA_DRIVER_CAPABILITIES="compute,video,utility" \
    PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR="/var/lib/plexmediaserver/Library/Application Support" \
    PLEX_MEDIA_SERVER_HOME="/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_MAX_PLUGIN_PROCS="6" \
    LD_LIBRARY_PATH="/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_INFO_VENDOR="Docker" \
    PLEX_MEDIA_SERVER_INFO_DEVICE="Docker Container (${VENDOR})"

# Start Manager
ENTRYPOINT ["/usr/local/bin/manager"]
