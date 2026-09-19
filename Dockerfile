ARG GO_VERSION=1.27
ARG VENDOR="machinectl"

# Stage 1: Build the Go manager and shim
FROM --platform=${BUILDPLATFORM} golang:${GO_VERSION} AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manager ./cmd/manager
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/shim ./cmd/shim
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/proxy ./cmd/proxy
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/maintenance ./cmd/maintenance

# Stage 1b: Build the PostgreSQL shim.
#
# This is the library that makes Plex talk to PostgreSQL instead of its own
# SQLite file. It interposes on Plex's SQLite symbols, so it has to be built
# against the same musl Plex bundles, which is why the base is pinned to Alpine
# 3.15 rather than something current. Upstream publishes no Linux binaries, so
# there is nothing to download instead.
FROM --platform=${BUILDPLATFORM} alpine:3.15 AS shim
# Our fork, not cgnl/plex-postgresql. Upstream's shim races with itself as soon
# as Plex uses it from more than one thread, which it does on every boot, and
# the maintainer has not answered a pull request since April 2026. Fixes go to
# the fork and come back here as a tag; see hack/plex-postgresql/README.md.
ARG PLEX_PG_REPO=https://github.com/mediactl/plex-postgresql
ARG PLEX_PG_REF=v1.3.17-clusterplex.8
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
# LD_PRELOAD and would load the interposer's constructor.
RUN sh scripts/docker-build-shim.sh --with-noop

# Stage 2: Extract Plex and set up the filesystem
FROM --platform=${BUILDPLATFORM} ubuntu:latest AS extractor
ARG TARGETARCH
ARG VENDOR
# Pinned, not latest. The PostgreSQL shim carries a schema dump taken from a
# particular Plex, and a server newer than that dump decides its full-text
# tables need rebuilding. It then issues CREATE VIRTUAL TABLE ... USING fts4
# with Plex's own collating tokenizer, which the shim's translator cannot
# parse, and Plex dies with an uncaught soci exception a fraction of a second
# after starting. This is the version the dump matches.
ARG VERSION=1.43.0.10492-121068a07

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

# Prepare the empty state directory Plex expects
RUN mkdir -p rootfs/var/lib/plexmediaserver

# Stage 3: Final image
FROM --platform=${BUILDPLATFORM} debian:bookworm-slim
# No FUSE, because the library is in PostgreSQL rather than a replicated file,
# and no iptables, because the manager builds Plex's namespace, veth pair and
# masquerade over netlink itself (ADR-0003).
#
# The rest are what upstream's own runtime installs, because we run upstream's
# initialisation script rather than reimplementing it: bash for the script,
# psql and sqlite3 for the databases it prepares, python3 for the helpers it
# may reach for.
RUN apt-get update && apt-get install -y \
      ca-certificates bash postgresql-client sqlite3 python3 \
    && rm -rf /var/lib/apt/lists/*

ARG VENDOR

# Our binaries
COPY --from=builder /app/bin/manager /usr/local/bin/manager
COPY --from=builder /app/bin/shim /usr/local/bin/shim
COPY --from=builder /app/bin/proxy /usr/local/bin/proxy
COPY --from=builder /app/bin/maintenance /usr/local/bin/maintenance

# The PostgreSQL shim and the libraries it links. The manager puts this on
# LD_PRELOAD when it starts Plex, which is what redirects Plex's database calls.
COPY --from=shim /libs/ /usr/local/lib/plex-postgresql/

# The subreaper adopts Plex's re-exec so it is not mistaken for an exit.
COPY --from=shim /libs/subreaper /usr/local/bin/subreaper

# The schema the shim expects to find already loaded. Plex does not create it:
# it runs migrations against a database that is meant to be there already, so
# without this it dies partway through insisting its own tables do not exist.
#
# Taken from our vendored copy rather than the upstream checkout, so a change
# to them is reviewable rather than arriving with a version bump.
COPY hack/plex-postgresql/schema/ /usr/local/lib/plex-postgresql/

# Upstream's own initialisation, run verbatim rather than reimplemented. It
# prepares the schema, the shadow databases and the directories Plex expects,
# and it is an s6 init script — it sets things up and exits without starting
# Plex, which is exactly the half we want. seed_shadow_table_from_pg.py is the
# helper it calls to copy `preferences` into the shadow.
#
# This is the standalone init, the one upstream's own non-linuxserver image
# runs, which is the flavour ours matches.
#
# migrate_lib.sh is deliberately NOT copied. The script offers to migrate a
# SQLite library it finds into PostgreSQL, and one of the places it looks is
# our own live database. The init guards every call to it with a file test, so
# leaving it out disables that path rather than breaking the script.
COPY hack/plex-postgresql/standalone-entrypoint.sh /usr/local/lib/plex-postgresql/
COPY hack/plex-postgresql/seed_shadow_table_from_pg.py /usr/local/lib/plex-postgresql/

# Copy the extracted Plex root filesystem over
COPY --from=extractor /plex-build/rootfs /

# After the Plex filesystem, because both of these live inside it.
#
# The shim is built against musl and asks for it by its Alpine soname, which is
# not what Plex calls its bundled copy. And Plex's CrashUploader is replaced by
# a no-op: it runs on every exit, cannot work here, and its failure raises
# SIGCHLD in a process that has the interposer loaded.
RUN ln -sf /usr/lib/plexmediaserver/lib/libc.so \
      "/usr/local/lib/plex-postgresql/libc.musl-$(uname -m).so.1"
COPY --from=shim /libs/noop /usr/lib/plexmediaserver/CrashUploader

# Plex drops privilege to this user when it spawns a plug-in, and the plug-in
# manager thread dies in the middle of the spawn if the lookup fails: the
# process appears, nothing is logged after "Plugin: setting environment
# variable: 'PYTHONPATH=...'", no plug-in ever reports its port and the server
# answers 503 for ever.
#
# plexinc/pms-docker creates the user; the .deb creates it in a postinst that
# `dpkg-deb -x` does not run, so extracting the package leaves no trace of it.
# Same uid and gid as the upstream image, so a volume written by one is
# readable by the other.
RUN groupadd --system --gid 1000 plex \
    && useradd --system --uid 1000 --gid plex --home-dir /config \
      --shell /bin/false plex

# Upstream's script addresses Plex's state as /config, the way the images it
# was written for do. Ours lives under /var/lib/plexmediaserver, so it is
# reachable by both names and the script needs no edits.
RUN ln -sfn /var/lib/plexmediaserver /config

# Set environment variables commonly required by Plex
ENV DEBIAN_FRONTEND="noninteractive" \
    NVIDIA_DRIVER_CAPABILITIES="compute,video,utility" \
    PLEX_MEDIA_SERVER_APPLICATION_SUPPORT_DIR="/var/lib/plexmediaserver/Library/Application Support" \
    PLEX_MEDIA_SERVER_HOME="/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_MAX_PLUGIN_PROCS="6" \
    LD_LIBRARY_PATH="/usr/local/lib/plex-postgresql:/usr/lib/plexmediaserver/lib:/usr/lib/plexmediaserver" \
    PLEX_MEDIA_SERVER_INFO_VENDOR="Docker" \
    PLEX_MEDIA_SERVER_INFO_DEVICE="Docker Container (${VENDOR})"

# Start Manager
ENTRYPOINT ["/usr/local/bin/manager"]
