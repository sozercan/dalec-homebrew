# syntax=docker/dockerfile:1.12

ARG GO_IMAGE=docker.io/library/golang:1.25.9-bookworm@sha256:298734aec230b5f3e8cee450ce6d7eccc39f1797ba548ee90d57e9803030c6c3
ARG RUNTIME_BASE=docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS ca-bundle

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS go-build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-materializer ./cmd/materializer && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-test-runner ./cmd/test-runner && \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags='-s -w -buildid=' -o /out/dalec-homebrew-record-verify ./cmd/record-verify

FROM ${RUNTIME_BASE} AS runtime-base
ARG TARGETARCH
ARG UBUNTU_SNAPSHOT=20260610T000000Z
ARG LINUXBREW_UID=1000
ARG LINUXBREW_GID=1000
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=ca-bundle /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
RUN (sed -i -E "s#https?://(archive.ubuntu.com|security.ubuntu.com|ports.ubuntu.com)/ubuntu(-ports)?/?#https://snapshot.ubuntu.com/ubuntu/${UBUNTU_SNAPSHOT}/#g" /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true) \
    && printf 'Acquire::Check-Valid-Until "false";\n' > /etc/apt/apt.conf.d/99snapshot \
    && apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates coreutils dash findutils grep sed \
    && rm -rf /var/lib/apt/lists/* \
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
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew /usr/share/dalec-homebrew \
    && install -d -o root -g root -m 0755 /home/linuxbrew/.linuxbrew/lib \
    && case "${TARGETARCH}" in \
         amd64) runtime_loader=/lib64/ld-linux-x86-64.so.2 ;; \
         arm64) runtime_loader=/lib/ld-linux-aarch64.so.1 ;; \
         *) echo "unsupported runtime loader architecture: ${TARGETARCH}" >&2; exit 1 ;; \
       esac \
    && test -e "${runtime_loader}" \
    && ln -s "${runtime_loader}" /home/linuxbrew/.linuxbrew/lib/ld.so \
    && dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' | LC_ALL=C sort > /usr/share/dalec-homebrew/runtime-base-packages.tsv \
    && find /home/linuxbrew -xdev -exec chown root:root {} + \
    && find /home/linuxbrew -xdev -type d -exec chmod 0755 {} +
ENV PATH=/home/linuxbrew/.linuxbrew/bin:/home/linuxbrew/.linuxbrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    HOME=/home/linuxbrew
USER linuxbrew
WORKDIR /home/linuxbrew

FROM runtime-base AS materializer
USER root
ARG HOMEBREW_COMMIT=77d90328ca2f63ff4ec1f67de0ade5632f5d2335
ARG HOMEBREW_ARCHIVE_SHA256=42e3678a8b00d53319f6b88b9384fcc7baa072e44864e41117cc7fd4f78fcb54
RUN apt-get update && apt-get install -y --no-install-recommends \
      binutils curl file git gzip install-info jq patchelf tar unzip util-linux xz-utils zstd \
    && rm -rf /var/lib/apt/lists/* \
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
    && chown -R root:root /home/linuxbrew/.linuxbrew/Homebrew \
    && chmod -R a-w /home/linuxbrew/.linuxbrew/Homebrew \
    && printf '[safe]\n\tdirectory = /home/linuxbrew/.linuxbrew/Homebrew\n' > /home/linuxbrew/.gitconfig \
    && chown root:root /home/linuxbrew/.gitconfig && chmod 0444 /home/linuxbrew/.gitconfig
COPY --from=go-build /out/dalec-homebrew-materializer /usr/local/bin/dalec-homebrew-materializer
COPY --from=go-build /out/dalec-homebrew-test-runner /usr/local/bin/dalec-homebrew-test-runner
COPY --from=go-build /out/dalec-homebrew-record-verify /usr/local/bin/dalec-homebrew-record-verify
COPY internal/materializer/pour.rb /usr/local/libexec/dalec-homebrew-pour.rb
COPY internal/homebrew/metadata/homebrew-1.pub /usr/share/dalec-homebrew/homebrew-1.pub
RUN chmod 0555 /usr/local/bin/dalec-homebrew-* \
    && chmod 0444 /usr/local/libexec/dalec-homebrew-pour.rb /usr/share/dalec-homebrew/homebrew-1.pub
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

FROM go-build AS frontend-build
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
COPY --from=go-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=frontend-build /out/dalec-homebrew-frontend /dalec-homebrew-frontend
COPY --from=go-build /out/dalec-homebrew-test-runner /dalec-homebrew-test-runner
LABEL moby.buildkit.frontend.caps="moby.buildkit.frontend.inputs"
ENTRYPOINT ["/dalec-homebrew-frontend"]
