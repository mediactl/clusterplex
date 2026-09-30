ARG GO_VERSION=1.27
ARG VENDOR="machinectl"

# Stage 1: Build the Go manager and shim.
#
# Cross-compiled for the target rather than built for the machine running the
# build: they are static (CGO_ENABLED=0), so the final image needs nothing for
# them but the binaries.
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
RUN go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim
RUN go build -trimpath -ldflags "-s -w" -o bin/proxy ./cmd/proxy
RUN go build -trimpath -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance

# Stage 1b: Build the PostgreSQL shim.
#
# This is the library that makes Plex talk to PostgreSQL instead of its own
# SQLite file. It interposes on Plex's SQLite symbols, so it has to be built
# against the same musl Plex bundles, which is why the base is pinned to Alpine
# 3.15 rather than something current. Upstream publishes no Linux binaries, so
# there is nothing to download instead.
#
# Built for the target platform, not the build platform: it is C and Rust
# linked against musl, and a musl cross toolchain is not set up here, so a
# cross build runs this stage under emulation.
FROM alpine:3.15 AS shim
# Our fork, not cgnl/plex-postgresql. Upstream's shim races with itself as soon
# as Plex uses it from more than one thread, which it does on every boot, and
# the maintainer has not answered a pull request since April 2026. Fixes go to
# the fork and come back here as a tag; see hack/plex-postgresql/README.md.
ARG PLEX_PG_REPO=https://github.com/mediactl/plex-postgresql
ARG PLEX_PG_REF=v1.3.17-clusterplex.22
RUN apk add --no-cache build-base sqlite-dev linux-headers curl perl git
WORKDIR /build
ENV CARGO_HOME=/usr/local/cargo \
    RUSTUP_HOME=/usr/local/rustup \
    CARGO_TARGET_DIR=/build/target \
    PATH="/usr/local/cargo/bin:${PATH}"
# Downloaded and run in two steps, not piped: in a pipeline the exit status is
# the shell's, so a failed download used to install nothing and still succeed,
# surfacing four layers later as "cargo: not found". The version check makes
# the failure land here instead.
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs -o /tmp/rustup.sh \
    && sh /tmp/rustup.sh -y --default-toolchain stable --profile minimal \
    && rm -f /tmp/rustup.sh \
    && cargo --version
# The build script addresses /build/rust by absolute path, so the checkout has
# to land at /build itself rather than in a subdirectory.
RUN git clone --quiet --depth 1 --branch ${PLEX_PG_REF} \
    ${PLEX_PG_REPO} /src \
    && cp -a /src/. /build/ && rm -rf /src
# --with-noop also builds a static no-op binary. It replaces Plex's
# CrashUploader below: a shell script would not do, because sh inherits
# LD_PRELOAD and would load the interposer's constructor. subreaper and noop
# are both linked statically, which is what lets them run in an image with no
# C library of its own.
RUN sh scripts/docker-build-shim.sh --with-noop

# Stage 2: Assemble the root filesystem.
#
# Everything the final image holds is put together here, where there is a
# shell, and copied across in one layer. The final image is `scratch`: nothing
# in it needs a distribution.
#
#   - Plex's own binaries carry their musl loader and every library they link
#     in /usr/lib/plexmediaserver/lib, so they need no system C library.
#   - Our binaries, subreaper and noop are static.
#   - The PostgreSQL shim is musl-linked and loads inside Plex, against Plex's
#     musl.
#   - What used to need bash, psql, sqlite3 and python3 -- upstream's init
#     script and its seeding helper -- is in the manager now
#     (pkg/plex/bootstrap).
FROM --platform=${BUILDPLATFORM} ubuntu:latest AS rootfs
ARG TARGETARCH
# Pinned, not latest, and moved deliberately rather than followed. A bump here
# is a change to the database, not to a download URL.
#
# The shim's schema dump carries its own record of which Plex migrations
# produced it. A server whose migration list runs past that record applies the
# outstanding ones at boot, and it also concludes that the library was written
# by an older server and rebuilds its full-text index. Both have broken this
# image before; see hack/plex-postgresql/README.md.
#
# Before moving it: boot the candidate on plain SQLite, read its
# schema_migrations, and diff that against the COPY block in
# hack/plex-postgresql/schema/plex_schema.sql. That is the exact list of
# migrations the upgrade will run. Then run the candidate against a fresh
# PostgreSQL loaded from the dump, with PLEX_PG_LOG_LEVEL=DEBUG, and read the
# shim's log — the index rebuild does not appear in the migration list at all.
#
# 1.43.0.10492 -> 1.43.4.10903 left two migrations outstanding, 202601121053
# and 202608120900, and both apply. The rebuild, and the vacuum and the planner
# statistics read that follow it, took three fixes in the shim — which is what
# v1.3.17-clusterplex.13 above carries.
ARG VERSION=1.43.4.10903-e5521bd8c

RUN apt-get update \
    && apt-get install -y --no-install-recommends wget ca-certificates tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build
RUN wget -q "https://downloads.plex.tv/plex-media-server-new/${VERSION}/debian/plexmediaserver_${VERSION}_${TARGETARCH}.deb" -O plex.deb \
    && dpkg-deb -x plex.deb deb \
    && rm plex.deb \
    && mkdir -p /rootfs/usr/lib \
    && mv deb/usr/lib/plexmediaserver /rootfs/usr/lib/plexmediaserver \
    && rm -rf deb

# Plex's helper binaries are replaced by links to our shim, which forwards
# each invocation to the manager; the real ones keep a .real suffix.
RUN cd /rootfs/usr/lib/plexmediaserver \
    && for b in "Plex Transcoder" "Plex Media Scanner" "Plex Commercial Skipper" "Plex Relay"; do \
         mv "$b" "$b.real" && ln -s /usr/local/bin/shim "$b"; \
       done

# The directories a container expects to find, and the ones Plex does.
#
# /config: the shim reads Plex's settings as
# /config/Library/Application Support/Plex Media Server/Preferences.xml,
# the path upstream's images mount. Ours lives under /var/lib/plexmediaserver,
# so it is reachable by both names.
RUN mkdir -p /rootfs/var/lib/plexmediaserver /rootfs/run /rootfs/etc/ssl/certs \
      /rootfs/usr/local/bin /rootfs/usr/local/lib/plex-postgresql /rootfs/usr/share \
    && mkdir -m 1777 /rootfs/tmp \
    && ln -s /var/lib/plexmediaserver /rootfs/config

# Plex drops privilege to this user when it spawns a plug-in, and the plug-in
# manager thread dies in the middle of the spawn if the lookup fails: the
# process appears, nothing is logged after "Plugin: setting environment
# variable: 'PYTHONPATH=...'", no plug-in ever reports its port and the server
# answers 503 for ever.
#
# plexinc/pms-docker creates the user; the .deb creates it in a postinst that
# `dpkg-deb -x` does not run, so extracting the package leaves no trace of it.
# Same uid and gid as the upstream image (and pkg/plex/bootstrap.PlexUID), so
# a volume written by one is readable by the other.
RUN printf '%s\n' \
      'root:x:0:0:root:/root:/sbin/nologin' \
      'plex:x:1000:1000:plex:/config:/bin/false' \
      'nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin' > /rootfs/etc/passwd \
    && printf '%s\n' 'root:x:0:' 'plex:x:1000:' 'nogroup:x:65534:' > /rootfs/etc/group

# The CA bundle, for our binaries and anything else that verifies TLS by the
# system path; Plex carries its own in Resources/cacert.pem. Zone data because
# Plex reads /usr/share/zoneinfo for a TZ it is given.
RUN cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/ \
    && cp -a /usr/share/zoneinfo /rootfs/usr/share/zoneinfo

# The shim is built against musl and asks for it by its Alpine soname, which is
# not what Plex calls its bundled copy.
RUN case "${TARGETARCH}" in \
      amd64) musl=x86_64 ;; \
      arm64) musl=aarch64 ;; \
      *) echo "unsupported architecture ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && ln -s /usr/lib/plexmediaserver/lib/libc.so \
      "/rootfs/usr/local/lib/plex-postgresql/libc.musl-${musl}.so.1"

# The PostgreSQL shim and the libraries it links. The manager puts this on
# LD_PRELOAD when it starts Plex, which is what redirects Plex's database calls.
COPY --from=shim /libs/*.so* /rootfs/usr/local/lib/plex-postgresql/
# The subreaper adopts Plex's re-exec so it is not mistaken for an exit.
COPY --from=shim /libs/subreaper /rootfs/usr/local/bin/subreaper
# Plex's CrashUploader is replaced by a no-op: it runs on every exit, cannot
# work here, and its failure raises SIGCHLD in a process that has the
# interposer loaded.
COPY --from=shim /libs/noop /rootfs/usr/lib/plexmediaserver/CrashUploader

# The SQL the manager loads before Plex starts (pkg/plex/bootstrap). Plex does
# not create its schema: it runs migrations against a database that is meant
# to be there already, so without it Plex dies partway through insisting its
# own tables do not exist.
#
# Taken from our vendored copy rather than the upstream checkout, so a change
# to them is reviewable rather than arriving with a version bump.
COPY hack/plex-postgresql/schema/ /rootfs/usr/local/lib/plex-postgresql/

# Our binaries.
COPY --from=builder /app/bin/manager /app/bin/shim /app/bin/proxy /app/bin/maintenance /rootfs/usr/local/bin/

# Stage 2b: A shell for the debug image.
#
# The musl build of busybox is static, so it runs in an image with no C
# library. The applets are symlinks rather than the hard links the busybox
# image ships, which COPY would turn into four hundred copies.
FROM busybox:1.37-musl AS busybox
RUN mkdir /out && cp /bin/busybox /out/busybox \
    && cd /out && for applet in $(./busybox --list); do \
         [ "$applet" = busybox ] || ln -s busybox "$applet"; \
       done

# Stage 3: The image.
#
# No FUSE, because the library is in PostgreSQL rather than a replicated file;
# no iptables, because the manager builds Plex's namespace, veth pair and
# masquerade over netlink itself (ADR-0003); and no shell, psql, sqlite3 or
# python3, because the manager prepares the databases itself.
FROM scratch AS base
ARG VENDOR
COPY --from=rootfs /rootfs/ /

ENV PATH="/usr/local/bin:/usr/bin:/bin" \
    NVIDIA_DRIVER_CAPABILITIES="compute,video,utility" \
    PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR="/var/lib/plexmediaserver/Library/Application Support" \
    PLEX_MEDIA_SERVER_HOME="/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_MAX_PLUGIN_PROCS="6" \
    LD_LIBRARY_PATH="/usr/local/lib/plex-postgresql:/usr/lib/plexmediaserver/lib:/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_INFO_VENDOR="Docker" \
    PLEX_MEDIA_SERVER_INFO_DEVICE="Docker Container (${VENDOR})"

ENTRYPOINT ["/usr/local/bin/manager"]

# The same image with busybox in /bin, for `kubectl exec` and for the
# end-to-end suite, which runs sh, cat and find inside the pod. Build it with
# `make docker-build DOCKER_TARGET=debug`.
#
# Plex itself behaves the same either way. Its plug-in host's Python calls
# /bin/sh when its uuid module looks for a C compiler (ctypes' find_library);
# without a shell that lookup fails, uuid.py catches the failure, and every
# plug-in starts as it does with one (checked against Plex 1.43.4 on
# 2026-09-30).
FROM base AS debug
COPY --from=busybox /out/ /bin/

# The default target, last so a plain `docker build` produces it.
FROM base AS release
