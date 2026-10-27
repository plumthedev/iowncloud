FROM debian:trixie-slim AS libs
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential autoconf automake libtool pkg-config git ca-certificates \
      libssl-dev libcurl4-openssl-dev \
    && rm -rf /var/lib/apt/lists/*

# libimobiledevice has no recent releases; everyone builds from git. Renovate bumps these.
# renovate: datasource=git-refs depName=libplist packageName=https://github.com/libimobiledevice/libplist currentValue=master
ARG LIBPLIST_REF=fe3dc34dc3484006e12b403f7ceb06f0ad40b6f4
# renovate: datasource=git-refs depName=libimobiledevice-glue packageName=https://github.com/libimobiledevice/libimobiledevice-glue currentValue=master
ARG LIBIMOBILEDEVICE_GLUE_REF=10ddafa16458a012bcbd3e83a6ab36a032dc84d8
# renovate: datasource=git-refs depName=libusbmuxd packageName=https://github.com/libimobiledevice/libusbmuxd currentValue=master
ARG LIBUSBMUXD_REF=93eb168bf6b07472d17781328c21df0c60300524
# renovate: datasource=git-refs depName=libtatsu packageName=https://github.com/libimobiledevice/libtatsu currentValue=master
ARG LIBTATSU_REF=e7d6ad13ef928aa609d0ccdfc586f7d6e8e049bf
# renovate: datasource=git-refs depName=libimobiledevice packageName=https://github.com/libimobiledevice/libimobiledevice currentValue=master
ARG LIBIMOBILEDEVICE_REF=fa0f79190142bc309307967c058f89c1b36eb6b8

# Full clones: autogen's git-version-gen needs tags.
RUN set -e; build() { \
      git clone -q "https://github.com/libimobiledevice/$1.git" "/src/$1"; \
      cd "/src/$1" && git checkout -q "$2"; \
      shift 2; ./autogen.sh --prefix=/usr/local --disable-static "$@" >/dev/null; \
      make -j"$(nproc)" >/dev/null && make install >/dev/null && ldconfig; \
    }; \
    build libplist "$LIBPLIST_REF" --without-cython; \
    build libimobiledevice-glue "$LIBIMOBILEDEVICE_GLUE_REF"; \
    build libusbmuxd "$LIBUSBMUXD_REF"; \
    build libtatsu "$LIBTATSU_REF"; \
    build libimobiledevice "$LIBIMOBILEDEVICE_REF" --without-cython

COPY tools/idevicewifi.c /src/
RUN cc -O2 -o /usr/local/bin/idevicewifi /src/idevicewifi.c \
      $(pkg-config --cflags --libs libimobiledevice-1.0 libplist-2.0)

FROM debian:trixie-slim AS netmuxd
ARG TARGETARCH
# Bump version and both checksums together (sha256 of the release tarballs).
ARG NETMUXD_VERSION=v0.4.3
ARG NETMUXD_SHA256_amd64=85b6598284fc639f2a282584461d05e2090b79bdf3ec949d2a5e5d3dc655dde4
ARG NETMUXD_SHA256_arm64=a89d587b094184b41431bc343da6dfa890391ccc7d4b3d6bce02b524f3191ae6
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN set -e; \
    case "$TARGETARCH" in amd64) a=x86_64; s="$NETMUXD_SHA256_amd64";; arm64) a=aarch64; s="$NETMUXD_SHA256_arm64";; *) exit 1;; esac; \
    curl -fsSL -o /n.tgz "https://github.com/jkcoxson/netmuxd/releases/download/${NETMUXD_VERSION}/netmuxd-${a}-unknown-linux-gnu.tar.gz"; \
    echo "$s  /n.tgz" | sha256sum -c -; \
    tar xzf /n.tgz -C /usr/local/bin netmuxd

# Go cross-compiles natively; no emulation needed for this stage.
FROM --platform=$BUILDPLATFORM golang:1.27-trixie AS app
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /iowncloud .

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      libssl3t64 libcurl4t64 tini ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=libs /usr/local/ /usr/local/
COPY --from=netmuxd /usr/local/bin/netmuxd /usr/local/bin/netmuxd
COPY --from=app /iowncloud /usr/local/bin/iowncloud
RUN ldconfig
ENV IOWNCLOUD_DATA=/data IOWNCLOUD_BACKUPS=/backups IOWNCLOUD_LISTEN=:8080
VOLUME ["/data", "/backups"]
EXPOSE 8080
ENTRYPOINT ["tini", "--"]
CMD ["iowncloud"]
