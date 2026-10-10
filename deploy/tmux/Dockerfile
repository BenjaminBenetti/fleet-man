# syntax=docker/dockerfile:1

# The static tmux bundle fleet ships beside the fleet-pod binary, for pod images
# that have no tmux of their own. fleet stages it at /opt/fleet/tmux and puts
# /opt/fleet/tmux/bin on PATH and /opt/fleet/tmux/share/terminfo on
# TERMINFO_DIRS, so sessions need neither root nor egress to a package mirror.
#
# Built against musl from pinned, checksummed sources, one build per platform.
# Build context is this directory:
#
#   docker buildx build --platform linux/arm64 --target bundle \
#     --output type=local,dest=dist deploy/tmux
#
# writes dist/fleet-tmux-linux-arm64.tar.gz (`make pod-tmux` wraps this). A
# foreign platform builds under QEMU (docker/setup-qemu-action in CI). The
# tarball is reproducible: sorted entries, zero mtimes, root owner, gzip -n.
#
# Bumping tmux or a library: change its version and sha256 below. The verify
# stage fails the build unless `tmux -V` reports TMUX_VERSION.

ARG ALPINE=alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG TMUX_VERSION=3.8
ARG TMUX_SHA256=e79c699c7e949dccd0a4a125e17b8d1261e311b16979dbc8eb34542f3966d82e
ARG LIBEVENT_VERSION=2.1.13-stable
ARG LIBEVENT_SHA256=f7e9383b8c0baa81b687e5b5eecc01beefaf1b19b64151d95ed61647fe7a315c
ARG NCURSES_VERSION=6.6
ARG NCURSES_SHA256=355b4cbbed880b0381a04c46617b7656e362585d52e9cf84a67e2009b749ff11

# The terminfo set: the TERM tmux gives its panes (tmux-256color), the
# fallbacks apps inside try, and the outer terminals a client attaches from.
ARG TERMINFO_ENTRIES=ansi,dumb,linux,vt100,vt102,vt220,xterm,xterm-color,xterm-256color,xterm-direct,screen,screen-256color,tmux,tmux-256color,tmux-direct,rxvt,st-256color,alacritty,foot,kitty,wezterm,ghostty,putty,putty-256color,konsole,konsole-256color,gnome,gnome-256color,iTerm2.app,ms-terminal,vscode,mintty,contour

# Where fleet stages the bundle in a pod. Compiled into ncurses as the last
# terminfo directory, so the bundled tmux finds its entries even when
# TERMINFO_DIRS is unset; the image's own databases come first.
ARG PREFIX=/opt/fleet/tmux

FROM ${ALPINE} AS build
RUN apk add --no-cache build-base bison pkgconf linux-headers tar gzip

ARG TMUX_VERSION TMUX_SHA256 LIBEVENT_VERSION LIBEVENT_SHA256 NCURSES_VERSION NCURSES_SHA256
WORKDIR /src
RUN set -eu; \
    fetch() { \
      out=$1; sum=$2; shift 2; \
      for url in "$@"; do wget -q -O "$out" "$url" && break; rm -f "$out"; done; \
      echo "$sum  $out" | sha256sum -c -; \
      mkdir "${out%.tar.gz}" && tar -xzf "$out" -C "${out%.tar.gz}" --strip-components=1; \
    }; \
    fetch ncurses.tar.gz "$NCURSES_SHA256" \
      "https://ftp.gnu.org/gnu/ncurses/ncurses-${NCURSES_VERSION}.tar.gz" \
      "https://invisible-island.net/archives/ncurses/ncurses-${NCURSES_VERSION}.tar.gz"; \
    fetch libevent.tar.gz "$LIBEVENT_SHA256" \
      "https://github.com/libevent/libevent/releases/download/release-${LIBEVENT_VERSION}/libevent-${LIBEVENT_VERSION}.tar.gz"; \
    fetch tmux.tar.gz "$TMUX_SHA256" \
      "https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz"

ARG PREFIX
ENV CFLAGS="-O2" PKG_CONFIG_PATH=/deps/lib/pkgconfig

# ncurses: static libraries plus tic (needed below); its own database is not
# installed, only the small set compiled from the source.
RUN cd ncurses && ./configure --prefix=/deps \
      --without-shared --with-normal --without-debug --without-profile \
      --without-cxx --without-cxx-binding --without-ada --without-manpages \
      --without-tests --enable-widec --enable-pc-files \
      --with-pkg-config-libdir=/deps/lib/pkgconfig --disable-db-install \
      --with-xterm-kbs=DEL \
      --with-default-terminfo-dir="${PREFIX}/share/terminfo" \
      --with-terminfo-dirs="/etc/terminfo:/lib/terminfo:/usr/share/terminfo:/usr/lib/terminfo:${PREFIX}/share/terminfo" \
    && make -j"$(nproc)" && make install

RUN cd libevent && ./configure --prefix=/deps \
      --disable-shared --enable-static --disable-openssl --disable-samples \
      --disable-libevent-regress --disable-debug-mode \
    && make -j"$(nproc)" && make install

RUN cd tmux && ./configure --prefix="${PREFIX}" --enable-static --with-TERM=tmux-256color \
    && make -j"$(nproc)" && make install-exec \
    && strip "${PREFIX}/bin/tmux"

# The terminfo set, from the same terminfo.src the database install would use
# (the misc/terminfo.tmp step applies --with-xterm-kbs). -x keeps the
# extended capabilities tmux reads (RGB, Tc, Smulx, ...).
ARG TERMINFO_ENTRIES
RUN set -eu; \
    make -C ncurses/misc terminfo.tmp; \
    mkdir -p "${PREFIX}/share/terminfo"; \
    /deps/bin/tic -x -o "${PREFIX}/share/terminfo" -e "${TERMINFO_ENTRIES}" ncurses/misc/terminfo.tmp; \
    for name in $(echo "${TERMINFO_ENTRIES}" | tr ',' ' '); do \
      first=$(printf '%.1s' "$name"); \
      test -f "${PREFIX}/share/terminfo/${first}/${name}" || { echo "missing terminfo entry: ${name}" >&2; exit 1; }; \
    done

# A static binary has no program interpreter and no shared-library deps.
RUN set -eu; bin="${PREFIX}/bin/tmux"; \
    if readelf -lW "$bin" | grep -q 'INTERP'; then echo "tmux is dynamically linked" >&2; exit 1; fi; \
    if readelf -dW "$bin" 2>/dev/null | grep -q 'NEEDED'; then echo "tmux needs shared libraries" >&2; exit 1; fi

ARG TARGETARCH
RUN set -eu; mkdir /out; \
    tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner --format=gnu \
      -C "${PREFIX}" -cf - bin share/terminfo | gzip -9n > "/out/fleet-tmux-linux-${TARGETARCH}.tar.gz"

# Unpack the bundle into a bare image (no tmux, no terminfo database) and run
# the smoke test against it, exactly as a pod would see it.
FROM ${ALPINE} AS verify
ARG PREFIX TMUX_VERSION TARGETARCH
COPY --from=build /out/ /out/
COPY smoke.sh /smoke.sh
RUN set -eu; mkdir -p "${PREFIX}"; \
    tar -xzf "/out/fleet-tmux-linux-${TARGETARCH}.tar.gz" -C "${PREFIX}"; \
    sh /smoke.sh "${PREFIX}" "${TMUX_VERSION}"

FROM scratch AS bundle
COPY --from=verify /out/ /
