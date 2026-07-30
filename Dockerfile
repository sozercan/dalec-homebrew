# syntax=docker/dockerfile:1.12

ARG GO_IMAGE=docker.io/library/golang:1.25.9-bookworm@sha256:298734aec230b5f3e8cee450ce6d7eccc39f1797ba548ee90d57e9803030c6c3
ARG RUNTIME_BASE=docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf

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

FROM go-source AS helper-build
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-materializer ./cmd/materializer && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-test-runner ./cmd/test-runner

FROM ${RUNTIME_BASE} AS runtime-base-rootfs
ARG TARGETARCH
ARG LINUXBREW_UID=1000
ARG LINUXBREW_GID=1000
COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=ca-bundle /usr/share/doc/ca-certificates/copyright /usr/share/doc/ca-certificates/copyright
COPY --from=ca-bundle /out/runtime-base-artifacts.tsv /usr/share/dalec-homebrew/runtime-base-artifacts.tsv
RUN printf '%s\n' \
      'path-exclude=/usr/share/doc/*' \
      'path-include=/usr/share/doc/*/copyright' \
      'path-exclude=/usr/share/man/*' \
      'path-exclude=/usr/share/info/*' \
      > /etc/dpkg/dpkg.cfg.d/01_nodoc \
    && existing_group="$(awk -F: -v id="${LINUXBREW_GID}" '$3 == id { print $1; exit }' /etc/group)" \
    && if [ -n "${existing_group}" ]; then \
         [ "${existing_group}" = linuxbrew ] || groupmod --new-name linuxbrew "${existing_group}"; \
       else \
         groupadd --gid "${LINUXBREW_GID}" linuxbrew; \
       fi \
    && existing_user="$(awk -F: -v id="${LINUXBREW_UID}" '$3 == id { print $1; exit }' /etc/passwd)" \
    && if [ -n "${existing_user}" ]; then \
         if [ "${existing_user}" != linuxbrew ]; then \
           usermod --login linuxbrew --home /home/linuxbrew --move-home --gid linuxbrew --shell /bin/bash "${existing_user}"; \
         else \
           usermod --home /home/linuxbrew --move-home --gid linuxbrew --shell /bin/bash linuxbrew; \
         fi; \
       else \
         useradd --uid "${LINUXBREW_UID}" --gid "${LINUXBREW_GID}" --home-dir /home/linuxbrew --create-home --shell /bin/bash linuxbrew; \
       fi \
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew/lib /usr/share/dalec-homebrew /usr/lib/ssl \
    && ln -s /etc/ssl/certs/ca-certificates.crt /usr/lib/ssl/cert.pem \
    && ln -s /etc/ssl/certs /usr/lib/ssl/certs \
    && case "${TARGETARCH}" in \
         amd64) runtime_loader=/lib64/ld-linux-x86-64.so.2 ;; \
         arm64) runtime_loader=/lib/ld-linux-aarch64.so.1 ;; \
         *) echo "unsupported runtime loader architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && test -e "${runtime_loader}" \
    && ln -s "${runtime_loader}" /home/linuxbrew/.linuxbrew/lib/ld.so \
    && dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' | LC_ALL=C sort > /usr/share/dalec-homebrew/runtime-base-packages.tsv \
    && find /home/linuxbrew -xdev -exec chown root:root {} + \
    && find /home/linuxbrew -xdev -type d -exec chmod 0755 {} + \
    && rm -rf /var/lib/apt/lists/* /var/cache/* /var/log/* /tmp/* /var/tmp/* /usr/share/man/* /usr/share/info/* \
    && find /usr/share/doc -type f ! -name copyright -delete \
    && find /usr/share/doc -depth -type d -empty -delete

FROM scratch AS runtime-base
COPY --from=runtime-base-rootfs / /
ENV PATH=/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/home/linuxbrew
LABEL org.opencontainers.image.version="24.04"
USER linuxbrew
WORKDIR /home/linuxbrew
CMD ["/bin/bash"]

FROM runtime-base AS materializer
USER root
ARG UBUNTU_SNAPSHOT=20260610T000000Z
ARG HOMEBREW_COMMIT=77d90328ca2f63ff4ec1f67de0ade5632f5d2335
ARG HOMEBREW_ARCHIVE_SHA256=42e3678a8b00d53319f6b88b9384fcc7baa072e44864e41117cc7fd4f78fcb54
ARG HOMEBREW_RUBY_VERSION=4.0.6
RUN (sed -i -E "s#https?://(archive.ubuntu.com|security.ubuntu.com|ports.ubuntu.com)/ubuntu(-ports)?/?#https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/#g" /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true) \
    && printf 'Acquire::Check-Valid-Until "false";\nAcquire::Retries "5";\n' > /etc/apt/apt.conf.d/99snapshot \
    && DEBIAN_FRONTEND=noninteractive apt-get update \
    && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      binutils curl file git install-info unzip xz-utils zstd \
    && rm -rf /var/lib/apt/lists/* \
    && install -d -o root -g root -m 0755 /usr/local/libexec \
    && curl --fail --location --proto '=https' --tlsv1.2 \
      "https://github.com/Homebrew/brew/archive/${HOMEBREW_COMMIT}.tar.gz" -o /tmp/brew.tar.gz \
    && echo "${HOMEBREW_ARCHIVE_SHA256}  /tmp/brew.tar.gz" | sha256sum -c - \
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew/Homebrew \
    && tar -xzf /tmp/brew.tar.gz --strip-components=1 -C /home/linuxbrew/.linuxbrew/Homebrew \
    && rm /tmp/brew.tar.gz \
    && chown linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew \
    && install -d -o linuxbrew -g linuxbrew -m 0755 \
      /home/linuxbrew/.linuxbrew/bin /home/linuxbrew/.linuxbrew/sbin /home/linuxbrew/.linuxbrew/lib \
      /home/linuxbrew/.linuxbrew/Cellar /home/linuxbrew/.linuxbrew/opt \
      /home/linuxbrew/.linuxbrew/etc /home/linuxbrew/.linuxbrew/var \
      /home/linuxbrew/.cache/Homebrew \
    && install -d -o root -g root -m 0555 \
      /home/linuxbrew/.linuxbrew/Homebrew/Library/Taps/homebrew/homebrew-core/Formula \
    && ln -s ../Homebrew/bin/brew /home/linuxbrew/.linuxbrew/bin/brew \
    && chown -h linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew/bin/brew \
    && chown -R linuxbrew:linuxbrew /home/linuxbrew/.linuxbrew/Homebrew/Library/Homebrew/vendor \
    && su -s /bin/bash -c 'HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_ANALYTICS=1 /home/linuxbrew/.linuxbrew/bin/brew vendor-install ruby' linuxbrew \
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
    && printf '[safe]\n\tdirectory = /home/linuxbrew/.linuxbrew/Homebrew\n' > /home/linuxbrew/.gitconfig \
    && chown root:root /home/linuxbrew/.gitconfig \
    && chmod 0444 /home/linuxbrew/.gitconfig
COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-materializer /usr/local/bin/dalec-homebrew-materializer
COPY --chmod=0555 --from=helper-build /out/dalec-homebrew-test-runner /usr/local/bin/dalec-homebrew-test-runner
COPY --chmod=0444 internal/materializer/pour.rb /usr/local/libexec/dalec-homebrew-pour.rb
ENV HOMEBREW_PREFIX=/home/linuxbrew/.linuxbrew \
    HOMEBREW_REPOSITORY=/home/linuxbrew/.linuxbrew/Homebrew \
    HOMEBREW_CELLAR=/home/linuxbrew/.linuxbrew/Cellar \
    HOMEBREW_CACHE=/home/linuxbrew/.cache/Homebrew \
    HOMEBREW_NO_AUTO_UPDATE=1 \
    HOMEBREW_NO_ANALYTICS=1 \
    HOMEBREW_NO_INSTALL_FROM_API=1 \
    HOMEBREW_NO_INSTALL_CLEANUP=1 \
    HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1
ENTRYPOINT ["/usr/local/bin/dalec-homebrew-materializer"]

FROM go-source AS frontend-build
ARG TARGETOS TARGETARCH
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
      -o /out/dalec-homebrew-frontend ./cmd/frontend

FROM scratch AS frontend
COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=frontend-build /out/dalec-homebrew-frontend /dalec-homebrew-frontend
LABEL moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"
ENTRYPOINT ["/dalec-homebrew-frontend"]
