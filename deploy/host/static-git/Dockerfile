# syntax=docker/dockerfile:1
# Reproducible static, non-PIE (ELF ET_EXEC) git for linux/amd64, built on musl.
# checkout-exec accepts only an ET_EXEC binary (internal/checkoutcontain isELF,
# cmd/checkout-exec validExecFD). Build with deploy/host/build-static-git.sh.

# alpine:3.23 pinned by index digest (musl toolchain).
FROM alpine:3.23@sha256:469c9ea410b9215eaee7976ba6c0639039b444be8e5d638f7abbada189971eb2 AS builder

ARG GIT_VERSION=2.56.1
# From https://mirrors.edge.kernel.org/pub/software/scm/git/sha256sums.asc
ARG GIT_SHA256=f0b0bf89264b9bd2e1c640363e7529643989da76281600473c50745cd34a607f

RUN apk add --no-cache build-base linux-headers binutils file curl xz perl \
        curl-dev curl-static openssl-dev openssl-libs-static zlib-dev zlib-static \
        nghttp2-static nghttp3-static brotli-static zstd-static libidn2-static libunistring-static \
        libpsl-static

WORKDIR /src
RUN curl -fsSLO "https://mirrors.edge.kernel.org/pub/software/scm/git/git-${GIT_VERSION}.tar.xz" \
 && echo "${GIT_SHA256}  git-${GIT_VERSION}.tar.xz" | sha256sum -c - \
 && tar -xJf "git-${GIT_VERSION}.tar.xz" \
 && mv "git-${GIT_VERSION}" git

# Alpine's static libidn2 bundles gnulib's error(), which collides with git's
# own error(). Rename the symbol inside a private copy of the archive (all of
# its definitions and references move together) and link against that copy.
RUN mkdir /idn2 && cp /usr/lib/libidn2.a /idn2/libidn2.a \
 && objcopy --redefine-sym error=idn2_gnu_error /idn2/libidn2.a

WORKDIR /src/git
# Install layout: git and its helpers live together in one dir so that git's
# own re-exec of "git <subcmd>" (e.g. index-pack during fetch) and of
# git-remote-https resolves there: gitexecdir=bin => exec path
# /usr/local/lib/autodeploy/bin (where install-checkout.sh puts git).
RUN make -j"$(nproc)" \
        prefix=/usr/local/lib/autodeploy bindir=/usr/local/lib/autodeploy/bin gitexecdir=bin \
        CFLAGS='-O2 -fno-pie -fno-stack-protector' LDFLAGS='-static -no-pie' \
        CURL_LDFLAGS="-L/idn2 $(curl-config --static-libs) -lbrotlicommon -lunistring" \
        NO_RUST=1 NO_PERL=1 NO_PYTHON=1 NO_TCLTK=1 NO_GETTEXT=1 NO_EXPAT=1 NO_ICONV=1 NO_REGEX=NeedsStartEnd \
        NO_INSTALL_HARDLINKS=1 NO_GITWEB=1 \
        git git-remote-http \
 && strip git git-remote-http \
 && mkdir /out && cp git /out/git && cp git-remote-http /out/git-remote-https

# Fail the build unless the binaries are static ET_EXEC with no PT_INTERP.
RUN set -eu; for f in /out/git /out/git-remote-https; do \
      [ "$(od -An -tx1 -j16 -N1 "$f" | tr -d ' ')" = 02 ] || { echo "$f: e_type != ET_EXEC"; exit 1; }; \
      if readelf -lW "$f" | grep -q INTERP; then echo "$f: has PT_INTERP"; exit 1; fi; \
    done

# Prints the verification evidence (docker run on this target).
FROM builder AS report
CMD ["sh", "-c", "for f in /out/git /out/git-remote-https; do echo \"== $f\"; file \"$f\"; readelf -hW \"$f\" | grep -E 'Type|Machine'; echo 'program headers:'; readelf -lW \"$f\" | grep -E 'Type|LOAD|INTERP|DYNAMIC' ; done"]

FROM scratch AS export
COPY --from=builder /out/git /out/git-remote-https /
