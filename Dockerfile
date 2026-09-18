# Stage 1: Build the Go Supervisor and Shim
FROM --platform=${BUILDPLATFORM} golang:1.21 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o bin/supervisor ./cmd/supervisor
RUN go build -o bin/shim ./cmd/shim

# Stage 2: Extract Plex and Setup File System
FROM --platform=${BUILDPLATFORM} ubuntu:22.04 AS extractor
ARG TARGETARCH
ARG VENDOR
ARG VERSION="1.43.4.10903-e5521bd8c"
RUN apt-get update && apt-get install -y wget xz-utils ca-certificates

WORKDIR /plex-build
RUN wget "https://downloads.plex.tv/plex-media-server-new/${VERSION}/debian/plexmediaserver_${VERSION}_${TARGETARCH}.deb" -O plex.deb && \
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
RUN mkdir -p rootfs/var/run rootfs/var/lib/litefs rootfs/var/lib/plexmediaserver

# Stage 3: Final Distroless Image
FROM --platform=${BUILDPLATFORM} gcr.io/distroless/cc-debian12

# Copy LiteFS and Custom Binaries
COPY --from=flyio/litefs:0.5 /usr/local/bin/litefs /usr/local/bin/litefs
COPY --from=builder /app/bin/supervisor /usr/local/bin/supervisor
COPY --from=builder /app/bin/shim /usr/local/bin/shim
COPY litefs.yml /etc/litefs.yml

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

# Start Supervisor
ENTRYPOINT ["/usr/local/bin/supervisor"]
