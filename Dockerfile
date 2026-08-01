# syntax=docker/dockerfile:1.12@sha256:93bfd3b68c109427185cd78b4779fc82b484b0b7618e36d0f104d4d801e66d25

ARG GO_IMAGE=docker.io/library/golang:1.25.9-bookworm@sha256:298734aec230b5f3e8cee450ce6d7eccc39f1797ba548ee90d57e9803030c6c3
# Full Ubuntu is retained only as the materializer's tooling environment.
ARG RUNTIME_BASE=docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf
ARG UBUNTU_SNAPSHOT=20260610T000000Z
ARG SOURCE_DATE_EPOCH=1781049600
ARG CHISEL_VERSION=1.4.2
ARG CHISEL_RELEASES_COMMIT=f42d76490045602d83de8afef5126987179a6693
ARG CHISEL_RELEASES_SHA256=d8f07a312d25a72d91dc48994fff7d3563cbb1bbb532a1affdbd61e12b35dc5b
ARG CHISEL_AMD64_SHA256=3b3d86ea38045e54a13334e358ab8da12e6dd33342163c5c1ba13525f1070cfe
ARG CHISEL_ARM64_SHA256=398050085cf32d7718ba1e2fad144b179e0943d0e0376b2a0614577c51d331e8

FROM ${GO_IMAGE} AS ca-bundle
ARG GO_IMAGE
RUN install -d /out \
    && version="$(dpkg-query -W -f='${Version}' ca-certificates)" \
    && bundle_sha256="$(sha256sum /etc/ssl/certs/ca-certificates.crt | cut -d ' ' -f 1)" \
    && printf 'deb\tdebian\tca-certificates\t%s\tall\t%s\tsha256:%s\t/etc/ssl/certs/ca-certificates.crt\n' \
      "${version}" "${GO_IMAGE}" "${bundle_sha256}" > /out/runtime-base-artifacts.tsv

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go-source
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .

FROM --platform=$BUILDPLATFORM go-source AS runtime-base-tool-build
ARG BUILDOS BUILDARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${BUILDOS} GOARCH=${BUILDARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-snapshot-proxy ./cmd/snapshot-proxy && \
    CGO_ENABLED=0 GOOS=${BUILDOS} GOARCH=${BUILDARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-runtime-base-evidence ./cmd/runtime-base-evidence

FROM go-source AS helper-build
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-materializer ./cmd/materializer && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-test-runner ./cmd/test-runner

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS runtime-base-rootfs
ARG BUILDARCH TARGETARCH
ARG UBUNTU_SNAPSHOT
ARG SOURCE_DATE_EPOCH
ARG CHISEL_VERSION
ARG CHISEL_RELEASES_COMMIT
ARG CHISEL_RELEASES_SHA256
ARG CHISEL_AMD64_SHA256
ARG CHISEL_ARM64_SHA256
ARG LINUXBREW_UID=1000
ARG LINUXBREW_GID=1000
COPY --from=runtime-base-tool-build /out/dalec-homebrew-snapshot-proxy /usr/local/bin/dalec-homebrew-snapshot-proxy
COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /runtime-input/ca-certificates.crt
COPY --from=ca-bundle /usr/share/doc/ca-certificates/copyright /runtime-input/ca-certificates-copyright
COPY --from=ca-bundle /out/runtime-base-artifacts.tsv /runtime-input/runtime-base-artifacts.tsv
COPY chisel/ubuntu-24.04/slices/ /runtime-input/chisel-slices/
RUN --mount=type=tmpfs,target=/root/.cache/chisel \
    set -eux; \
    case "${BUILDARCH}" in \
      amd64) chisel_sha256="${CHISEL_AMD64_SHA256}" ;; \
      arm64) chisel_sha256="${CHISEL_ARM64_SHA256}" ;; \
      *) echo "unsupported Chisel build architecture: ${BUILDARCH}" >&2; exit 1 ;; \
    esac; \
    install -d /rootfs /tmp/chisel-release; \
    curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/canonical/chisel/releases/download/v${CHISEL_VERSION}/chisel_v${CHISEL_VERSION}_linux_${BUILDARCH}.tar.gz" \
      -o /tmp/chisel.tar.gz; \
    echo "${chisel_sha256}  /tmp/chisel.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/chisel.tar.gz -C /usr/local/bin chisel; \
    curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/canonical/chisel-releases/archive/${CHISEL_RELEASES_COMMIT}.tar.gz" \
      -o /tmp/chisel-releases.tar.gz; \
    echo "${CHISEL_RELEASES_SHA256}  /tmp/chisel-releases.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/chisel-releases.tar.gz --strip-components=1 -C /tmp/chisel-release; \
    cp /runtime-input/chisel-slices/*.yaml /tmp/chisel-release/slices/; \
    dalec-homebrew-snapshot-proxy \
      --snapshot "${UBUNTU_SNAPSHOT}" --listen 127.0.0.1:18080 --ready-file /tmp/chisel-proxy.ready & \
    proxy_pid=$!; \
    trap 'kill "${proxy_pid}" 2>/dev/null || true; wait "${proxy_pid}" 2>/dev/null || true' EXIT; \
    for attempt in 1 2 3 4 5 6 7 8 9 10; do test -s /tmp/chisel-proxy.ready && break; sleep 1; done; \
    test -s /tmp/chisel-proxy.ready; \
    NO_PROXY= no_proxy= HTTP_PROXY=http://127.0.0.1:18080 http_proxy=http://127.0.0.1:18080 \
      chisel cut --release /tmp/chisel-release --root /rootfs --arch "${TARGETARCH}" \
        base-files_base base-files_release-info base-files_chisel \
        base-passwd_data \
        bash_bins bash_config dash_bins \
        coreutils_bins debianutils_which diffutils_bins findutils_bins grep_bins sed_bins mawk_bins \
        gzip_scripts hostname_bins ncurses-bin_bins tar_bins perl-base_bins perl-base_modules perl-base_unicore \
        procps_bins sensible-utils_bins util-linux_cli-helpers util-linux_lock util-linux_process \
        libc-bin_nsswitch libc-bin_locale libc-bin_getconf libc-bin_iconv libc-bin_getent \
        libc6_gconv libgcc-s1_libs libstdc++6_libs ncurses-base_terminfo \
        netbase_default-hosts netbase_default-networks tzdata_zoneinfo; \
    kill "${proxy_pid}"; wait "${proxy_pid}"; trap - EXIT; \
    rm -rf /tmp/chisel.tar.gz /tmp/chisel-releases.tar.gz /tmp/chisel-release /tmp/chisel-proxy.ready
COPY --from=runtime-base-tool-build /out/dalec-homebrew-runtime-base-evidence /usr/local/bin/dalec-homebrew-runtime-base-evidence
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) runtime_loader=/lib64/ld-linux-x86-64.so.2; target_loader=/rootfs/usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 ;; \
      arm64) runtime_loader=/lib/ld-linux-aarch64.so.1; target_loader=/rootfs/usr/lib/aarch64-linux-gnu/ld-linux-aarch64.so.1 ;; \
      *) echo "unsupported runtime loader architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    test -e "${target_loader}"; \
    dalec-homebrew-runtime-base-evidence \
      --manifest /rootfs/var/lib/chisel/manifest.wall --root /rootfs \
      --inventory /rootfs/usr/share/dalec-homebrew/runtime-base-packages.tsv; \
    if awk -F: -v id="${LINUXBREW_GID}" '$3 == id { found=1 } END { exit !found }' /rootfs/etc/group; then \
      echo "Chisel base already contains GID ${LINUXBREW_GID}" >&2; exit 1; \
    fi; \
    if awk -F: -v id="${LINUXBREW_UID}" '$3 == id { found=1 } END { exit !found }' /rootfs/etc/passwd; then \
      echo "Chisel base already contains UID ${LINUXBREW_UID}" >&2; exit 1; \
    fi; \
    printf 'linuxbrew:x:%s:\n' "${LINUXBREW_GID}" >> /rootfs/etc/group; \
    printf 'linuxbrew:x:%s:%s:Linuxbrew:/home/linuxbrew:/bin/bash\n' "${LINUXBREW_UID}" "${LINUXBREW_GID}" >> /rootfs/etc/passwd; \
    install -d -o root -g root -m 0755 \
      /rootfs/home/linuxbrew/.linuxbrew/lib /rootfs/etc/ssl/certs /rootfs/usr/lib/ssl \
      /rootfs/usr/share/dalec-homebrew /rootfs/usr/share/doc/ca-certificates; \
    install -o root -g root -m 0444 /runtime-input/ca-certificates.crt /rootfs/etc/ssl/certs/ca-certificates.crt; \
    install -o root -g root -m 0444 /runtime-input/ca-certificates-copyright /rootfs/usr/share/doc/ca-certificates/copyright; \
    install -o root -g root -m 0444 /runtime-input/runtime-base-artifacts.tsv /rootfs/usr/share/dalec-homebrew/runtime-base-artifacts.tsv; \
    ln -s /etc/ssl/certs/ca-certificates.crt /rootfs/usr/lib/ssl/cert.pem; \
    ln -s /etc/ssl/certs /rootfs/usr/lib/ssl/certs; \
    ln -s mawk /rootfs/usr/bin/awk; \
    ln -s "${runtime_loader}" /rootfs/home/linuxbrew/.linuxbrew/lib/ld.so; \
    mv /rootfs/var/lib/chisel/manifest.wall /rootfs/usr/share/dalec-homebrew/runtime-base-chisel.manifest.wall; \
    rmdir /rootfs/var/lib/chisel; \
    test "$(awk -F: '/^linuxbrew:/ { print $3 }' /rootfs/etc/passwd)" = "${LINUXBREW_UID}"; \
    test "$(awk -F: '/^linuxbrew:/ { print $4 }' /rootfs/etc/passwd)" = "${LINUXBREW_GID}"; \
    test "$(awk -F: '/^linuxbrew:/ { print $3 }' /rootfs/etc/group)" = "${LINUXBREW_GID}"; \
    test "$(stat -c '%u:%g:%a' /rootfs/home/linuxbrew)" = '0:0:755'; \
    test "$(stat -c '%u:%g:%a' /rootfs/home/linuxbrew/.linuxbrew)" = '0:0:755'; \
    test -x /rootfs/bin/bash; test -x /rootfs/bin/sh; test -x /rootfs/usr/bin/env; \
    test -x /rootfs/usr/bin/getent; test -x /rootfs/usr/bin/perl; \
    test -x /rootfs/usr/bin/gunzip; test -x /rootfs/usr/bin/hostname; test -x /rootfs/usr/bin/tput; \
    test ! -e /rootfs/usr/bin/apt; test ! -e /rootfs/usr/bin/dpkg; \
    test ! -e /rootfs/var/lib/dpkg/status; \
    case "${SOURCE_DATE_EPOCH}" in ''|*[!0-9]*) echo "invalid SOURCE_DATE_EPOCH" >&2; exit 1 ;; esac; \
    find /rootfs -xdev -exec touch -h -d "@${SOURCE_DATE_EPOCH}" {} +

FROM scratch AS runtime-base
COPY --from=runtime-base-rootfs /rootfs/ /
ENV PATH=/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/home/linuxbrew \
    LANG=C.UTF-8
LABEL org.opencontainers.image.version="24.04" \
      org.opencontainers.image.base.name="Ubuntu Chiseled 24.04"
USER linuxbrew
WORKDIR /home/linuxbrew
CMD ["/bin/bash"]

FROM ${RUNTIME_BASE} AS materializer-rootfs
USER root
ARG TARGETARCH
ARG UBUNTU_SNAPSHOT
ARG SOURCE_DATE_EPOCH
ARG LINUXBREW_UID=1000
ARG LINUXBREW_GID=1000
ARG HOMEBREW_COMMIT=77d90328ca2f63ff4ec1f67de0ade5632f5d2335
ARG HOMEBREW_ARCHIVE_SHA256=42e3678a8b00d53319f6b88b9384fcc7baa072e44864e41117cc7fd4f78fcb54
ARG HOMEBREW_RUBY_VERSION=4.0.6
RUN --mount=from=runtime-base-rootfs,source=/rootfs,target=/run/runtime-base,ro \
    install -D -o root -g root -m 0444 /run/runtime-base/etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt \
    && install -D -o root -g root -m 0444 /run/runtime-base/usr/share/doc/ca-certificates/copyright /usr/share/doc/ca-certificates/copyright \
    && install -D -o root -g root -m 0444 /run/runtime-base/usr/share/dalec-homebrew/runtime-base-packages.tsv /usr/share/dalec-homebrew/runtime-base-packages.tsv \
    && install -D -o root -g root -m 0444 /run/runtime-base/usr/share/dalec-homebrew/runtime-base-artifacts.tsv /usr/share/dalec-homebrew/runtime-base-artifacts.tsv \
    && printf '%s\n' \
      'path-exclude=/usr/share/doc/*' \
      'path-include=/usr/share/doc/*/copyright' \
      'path-exclude=/usr/share/man/*' \
      'path-exclude=/usr/share/info/*' \
      > /etc/dpkg/dpkg.cfg.d/01_nodoc \
    && existing_group="$(awk -F: -v id="${LINUXBREW_GID}" '$3 == id { print $1; exit }' /etc/group)" \
    && if [ -n "${existing_group}" ]; then \
         [ "${existing_group}" = linuxbrew ] || groupmod --new-name linuxbrew "${existing_group}"; \
       else groupadd --gid "${LINUXBREW_GID}" linuxbrew; fi \
    && existing_user="$(awk -F: -v id="${LINUXBREW_UID}" '$3 == id { print $1; exit }' /etc/passwd)" \
    && if [ -n "${existing_user}" ]; then \
         if [ "${existing_user}" != linuxbrew ]; then \
           usermod --login linuxbrew --home /home/linuxbrew --move-home --gid linuxbrew --shell /bin/bash "${existing_user}"; \
         else usermod --home /home/linuxbrew --move-home --gid linuxbrew --shell /bin/bash linuxbrew; fi; \
       else useradd --uid "${LINUXBREW_UID}" --gid "${LINUXBREW_GID}" --home-dir /home/linuxbrew --create-home --shell /bin/bash linuxbrew; fi \
    && usermod -G linuxbrew linuxbrew \
    && test "$(id -u linuxbrew):$(id -g linuxbrew):$(id -Gn linuxbrew)" = "${LINUXBREW_UID}:${LINUXBREW_GID}:linuxbrew" \
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew/lib /usr/local/libexec /etc/homebrew \
    && case "${TARGETARCH}" in \
         amd64) runtime_loader=/lib64/ld-linux-x86-64.so.2 ;; \
         arm64) runtime_loader=/lib/ld-linux-aarch64.so.1 ;; \
         *) echo "unsupported runtime loader architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && test -e "${runtime_loader}" \
    && ln -s "${runtime_loader}" /home/linuxbrew/.linuxbrew/lib/ld.so \
    && find /home/linuxbrew -xdev -exec chown root:root {} + \
    && find /home/linuxbrew -xdev -type d -exec chmod 0755 {} + \
    && test "$(stat -c '%u:%g:%a' /home/linuxbrew/.linuxbrew)" = '0:0:755' \
    && (sed -i -E "s#https?://(archive.ubuntu.com|security.ubuntu.com|ports.ubuntu.com)/ubuntu(-ports)?/?#https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/#g" /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true) \
    && printf 'Acquire::Check-Valid-Until "false";\nAcquire::Retries "5";\n' > /etc/apt/apt.conf.d/99snapshot \
    && DEBIAN_FRONTEND=noninteractive apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      binutils curl file git install-info unzip xz-utils zstd \
    && rm -rf /var/lib/apt/lists/* \
    && printf 'HOMEBREW_SYSTEM_ENV_TAKES_PRIORITY=1\nHOMEBREW_BASH_COMMAND=\n' > /etc/homebrew/brew.env \
    && chown root:root /etc/homebrew/brew.env \
    && chmod 0444 /etc/homebrew/brew.env \
    && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/Homebrew/brew/archive/${HOMEBREW_COMMIT}.tar.gz" -o /tmp/brew.tar.gz \
    && echo "${HOMEBREW_ARCHIVE_SHA256}  /tmp/brew.tar.gz" | sha256sum -c - \
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew/Homebrew \
    && tar -xzf /tmp/brew.tar.gz --strip-components=1 -C /home/linuxbrew/.linuxbrew/Homebrew \
    && rm /tmp/brew.tar.gz \
    && mv /home/linuxbrew/.linuxbrew/Homebrew/bin/brew /home/linuxbrew/.linuxbrew/Homebrew/bin/brew.real \
    && printf '%s\n' '#!/bin/bash -p' 'exec /bin/bash -p -c '"'"'source /home/linuxbrew/.linuxbrew/Homebrew/bin/brew.real'"'"' /home/linuxbrew/.linuxbrew/.dalec-homebrew/brew "$@"' \
      > /home/linuxbrew/.linuxbrew/Homebrew/bin/brew \
    && chmod 0555 /home/linuxbrew/.linuxbrew/Homebrew/bin/brew /home/linuxbrew/.linuxbrew/Homebrew/bin/brew.real \
    && install -d -o root -g root -m 0555 /home/linuxbrew/.linuxbrew/.dalec-homebrew \
    && ln -s ../Homebrew/bin/brew.real /home/linuxbrew/.linuxbrew/.dalec-homebrew/brew \
    && chown -h root:root /home/linuxbrew/.linuxbrew/.dalec-homebrew/brew \
    && chown linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew \
    && install -d -o linuxbrew -g linuxbrew -m 0755 \
      /home/linuxbrew/.linuxbrew/bin /home/linuxbrew/.linuxbrew/sbin /home/linuxbrew/.linuxbrew/lib \
      /home/linuxbrew/.linuxbrew/include /home/linuxbrew/.linuxbrew/share \
      /home/linuxbrew/.linuxbrew/Cellar /home/linuxbrew/.linuxbrew/Caskroom \
      /home/linuxbrew/.linuxbrew/Frameworks /home/linuxbrew/.linuxbrew/opt \
      /home/linuxbrew/.linuxbrew/etc /home/linuxbrew/.linuxbrew/var \
      /home/linuxbrew/.cache/Homebrew \
    && install -d -o root -g root -m 0555 \
      /home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/homebrew/homebrew-core \
    && ln -s ../Homebrew/bin/brew /home/linuxbrew/.linuxbrew/bin/brew \
    && chown -h linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew/bin/brew \
    && chown -R linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew/vendor \
    && su -s /bin/bash -c 'HOMEBREW_PREFIX=/home/linuxbrew/.linuxbrew HOMEBREW_REPOSITORY=/home/linuxbrew/.linuxbrew/Homebrew HOMEBREW_CELLAR=/home/linuxbrew/.linuxbrew/Cellar HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 /home/linuxbrew/.linuxbrew/Homebrew/bin/brew vendor-install ruby' linuxbrew \
    && repo=/home/linuxbrew/.linuxbrew/Homebrew \
    && ruby_root="${repo}/Library/Homebrew/vendor/portable-ruby/${HOMEBREW_RUBY_VERSION}" \
    && test -x "${ruby_root}/bin/ruby" \
    && rm -rf /home/linuxbrew/.cache/Homebrew/* \
      "${repo}/.github" "${repo}/.vscode" "${repo}/docs" "${repo}/manpages" "${repo}/completions" "${repo}/package" \
      "${repo}/Library/Homebrew/test" "${repo}/Library/Homebrew/test_bot" "${repo}/Library/Homebrew/sorbet" \
      "${repo}/Library/Homebrew/rubocops" "${repo}/Library/Homebrew/yard" \
      "${ruby_root}/include" "${ruby_root}/lib/pkgconfig" "${ruby_root}/share/man" \
    && find "${ruby_root}/lib" -maxdepth 1 -type f -name '*.a' -delete \
    && find "${ruby_root}/lib/ruby/gems" -type d -name cache -prune -exec rm -rf {} + \
    && for duplicate in scalar git-shell; do \
         cmp -s "/usr/bin/${duplicate}" "/usr/lib/git-core/${duplicate}" \
           && rm "/usr/lib/git-core/${duplicate}" \
           && ln "/usr/bin/${duplicate}" "/usr/lib/git-core/${duplicate}"; \
       done \
    && cmp -s /usr/bin/git /usr/lib/git-core/git \
    && rm /usr/lib/git-core/git \
    && ln /usr/bin/git /usr/lib/git-core/git \
    && rm -rf /var/cache/* /var/log/* /tmp/* /var/tmp/* /usr/share/man/* /usr/share/info/* \
    && find /usr/share/doc -type f ! -name copyright -delete \
    && find /usr/share/doc -depth -type d -empty -delete \
    && chown -R root:root /home/linuxbrew/.linuxbrew/Homebrew \
    && chmod -R a-w /home/linuxbrew/.linuxbrew/Homebrew \
    && chown root:root /home/linuxbrew/.linuxbrew \
    && chmod 0755 /home/linuxbrew/.linuxbrew \
    && test "$(stat -c '%u:%g:%a' /home/linuxbrew/.linuxbrew)" = '0:0:755' \
    && test ! -e /home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/homebrew/homebrew-core/Formula \
    && test -x /home/linuxbrew/.linuxbrew/Homebrew/bin/brew \
    && test -x /home/linuxbrew/.linuxbrew/Homebrew/bin/brew.real \
    && test "$(readlink /home/linuxbrew/.linuxbrew/.dalec-homebrew/brew)" = '../Homebrew/bin/brew.real' \
    && if su -s /bin/sh -c 'mv /home/linuxbrew/.linuxbrew /home/linuxbrew/.linuxbrew.swap' linuxbrew >/dev/null 2>&1; then \
         echo 'linuxbrew can replace the protected Homebrew prefix' >&2; exit 1; \
       fi \
    && if su -s /bin/sh -c 'mv /home/linuxbrew/.linuxbrew/Homebrew /home/linuxbrew/.linuxbrew/Homebrew.swap' linuxbrew >/dev/null 2>&1; then \
         echo 'linuxbrew can replace the protected Homebrew repository' >&2; exit 1; \
       fi \
    && printf '[safe]\n\tdirectory = /home/linuxbrew/.linuxbrew/Homebrew\n' > /home/linuxbrew/.gitconfig \
    && chown root:root /home/linuxbrew/.gitconfig \
    && chmod 0444 /home/linuxbrew/.gitconfig
COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-materializer /usr/local/bin/dalec-homebrew-materializer
COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-test-runner /usr/local/bin/dalec-homebrew-test-runner
COPY --chmod=0444 internal/materializer/pour.rb /usr/local/libexec/dalec-homebrew-pour.rb
RUN set -eu; \
    case "${SOURCE_DATE_EPOCH}" in ''|*[!0-9]*) echo "invalid SOURCE_DATE_EPOCH" >&2; exit 1 ;; esac; \
    find / -xdev \
      \( -path /dev -o -path /proc -o -path /sys \
         -o -path /etc/hostname -o -path /etc/hosts -o -path /etc/mtab -o -path /etc/resolv.conf \) -prune -o \
      -exec touch -h -d "@${SOURCE_DATE_EPOCH}" {} +; \
    dedupe() { source="$1"; shift; for duplicate in "$@"; do cmp -s "${source}" "${duplicate}"; rm "${duplicate}"; ln "${source}" "${duplicate}"; done; }; \
    dedupe /usr/bin/gunzip /usr/bin/uncompress; \
    dedupe /usr/bin/perl /usr/bin/perl5.38.2; \
    dedupe /usr/bin/git /usr/lib/git-core/git; \
    dedupe /usr/bin/git-shell /usr/lib/git-core/git-shell; \
    dedupe /usr/bin/perlbug /usr/bin/perlthanks; \
    dedupe /usr/bin/scalar /usr/lib/git-core/scalar; \
    dedupe /usr/bin/unzip /usr/bin/zipinfo; \
    printf '%s\t%s\n' \
      /usr/bin/gunzip /usr/bin/uncompress \
      /usr/bin/perl /usr/bin/perl5.38.2 \
      /usr/bin/git /usr/lib/git-core/git \
      /usr/bin/git-shell /usr/lib/git-core/git-shell \
      /usr/bin/perlbug /usr/bin/perlthanks \
      /usr/bin/scalar /usr/lib/git-core/scalar \
      /usr/bin/unzip /usr/bin/zipinfo \
      > /usr/share/dalec-homebrew/materializer-hardlinks.tsv; \
    chmod 0444 /usr/share/dalec-homebrew/materializer-hardlinks.tsv; \
    touch -h -d "@${SOURCE_DATE_EPOCH}" \
      /usr/bin /usr/lib/git-core /usr/share/dalec-homebrew /usr/share/dalec-homebrew/materializer-hardlinks.tsv

FROM scratch AS materializer
COPY --from=materializer-rootfs / /
USER root
ENV PATH=/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/home/linuxbrew \
    HOMEBREW_PREFIX=/home/linuxbrew/.linuxbrew \
    HOMEBREW_REPOSITORY=/home/linuxbrew/.linuxbrew/Homebrew \
    HOMEBREW_CELLAR=/home/linuxbrew/.linuxbrew/Cellar \
    HOMEBREW_CACHE=/home/linuxbrew/.cache/Homebrew \
    HOMEBREW_NO_AUTO_UPDATE=1 \
    HOMEBREW_NO_ANALYTICS=1 \
    HOMEBREW_NO_INSTALL_FROM_API=1 \
    HOMEBREW_NO_INSTALL_CLEANUP=1 \
    HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1
WORKDIR /home/linuxbrew
ENTRYPOINT ["/usr/local/bin/dalec-homebrew-materializer"]

FROM go-source AS frontend-build
ARG TARGETOS TARGETARCH
ARG SOURCE_DATE_EPOCH
ARG RUNTIME_BASE_REF
ARG MATERIALIZER_REF
ARG FRONTEND_REF
ARG HOMEBREW_COMMIT=77d90328ca2f63ff4ec1f67de0ade5632f5d2335
ARG HOMEBREW_KEYS_DIGEST=sha256:ef2d2c9e0219d485df9f07fff7b037feadc36c93085be9ffefb1390f31a3de1d
ARG HOMEBREW_RUBY_VERSION=4.0.6
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags="-s -w -buildid= \
      -X github.com/sozercan/dalec-homebrew/internal/config.RuntimeBaseRef=${RUNTIME_BASE_REF} \
      -X github.com/sozercan/dalec-homebrew/internal/config.MaterializerRef=${MATERIALIZER_REF} \
      -X github.com/sozercan/dalec-homebrew/internal/config.FrontendRef=${FRONTEND_REF} \
      -X github.com/sozercan/dalec-homebrew/internal/config.HomebrewCommit=${HOMEBREW_COMMIT} \
      -X github.com/sozercan/dalec-homebrew/internal/config.VerificationKeysDigest=${HOMEBREW_KEYS_DIGEST} \
      -X github.com/sozercan/dalec-homebrew/internal/config.PortableRubyVersion=${HOMEBREW_RUBY_VERSION}" \
      -o /out/dalec-homebrew-frontend ./cmd/frontend \
    && touch -d "@${SOURCE_DATE_EPOCH}" /out/dalec-homebrew-frontend

FROM scratch AS frontend
COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=frontend-build /out/dalec-homebrew-frontend /dalec-homebrew-frontend
LABEL moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"
ENTRYPOINT ["/dalec-homebrew-frontend"]
