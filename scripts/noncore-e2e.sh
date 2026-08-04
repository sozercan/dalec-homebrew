#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

BUILDKIT_IMAGE=${DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE:-}
REGISTRY_IMAGE=${DALEC_HOMEBREW_E2E_REGISTRY_IMAGE:-}
NGINX_IMAGE=${DALEC_HOMEBREW_E2E_NGINX_IMAGE:-}
RUN_ID=${DALEC_HOMEBREW_E2E_RUN_ID:-$(date -u +%Y%m%d%H%M%S)}
FINAL_IMAGE=${DALEC_HOMEBREW_E2E_IMAGE:-dalec-homebrew-noncore-e2e:dev}
SPEC=${DALEC_HOMEBREW_E2E_SPEC:-examples/ci-noncore-multi-package.yaml}
PROGRESS=${DALEC_HOMEBREW_E2E_PROGRESS:-plain}
PLATFORM=${DALEC_HOMEBREW_E2E_PLATFORM:-linux/amd64}
KEEP_WORK=${DALEC_HOMEBREW_E2E_KEEP_WORK:-0}

fail_usage() {
  echo "$*" >&2
  exit 64
}

for tool in docker jq go openssl curl base64; do
  command -v "$tool" >/dev/null 2>&1 || fail_usage "required tool is unavailable: $tool"
done
[[ -n "$BUILDKIT_IMAGE" ]] || fail_usage "DALEC_HOMEBREW_E2E_BUILDKIT_IMAGE is required"
[[ -n "$REGISTRY_IMAGE" ]] || fail_usage "DALEC_HOMEBREW_E2E_REGISTRY_IMAGE is required"
[[ -n "$NGINX_IMAGE" ]] || fail_usage "DALEC_HOMEBREW_E2E_NGINX_IMAGE is required"
[[ "$PLATFORM" == linux/amd64 ]] || fail_usage "only linux/amd64 is supported by the CI non-core E2E"
[[ "$RUN_ID" =~ ^[0-9A-Za-z][0-9A-Za-z_.-]{0,63}$ ]] || fail_usage "invalid E2E run ID"
[[ -f "$SPEC" ]] || fail_usage "E2E spec does not exist: $SPEC"

WORK=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/dalec-homebrew-noncore-e2e.XXXXXX")
NETWORK="dalec-homebrew-e2e-${RUN_ID}"
REGISTRY_CONTAINER="dalec-homebrew-e2e-registry-${RUN_ID}"
BUILDER="dalec-homebrew-e2e-${RUN_ID}"
INGESTION_CONTAINER="dalec-homebrew-catalog-worker-${RUN_ID}"
SERVICE_CONTAINER="dalec-homebrew-catalog-http-${RUN_ID}"
PROXY_CONTAINER="dalec-homebrew-catalog-proxy-${RUN_ID}"
REGISTRY="e2e-registry:5000"
HOST_REGISTRY="localhost:5000"
CATALOG_ORIGIN="https://${PROXY_CONTAINER}"
KEY_ID="catalog-e2e-${RUN_ID}"

cleanup() {
  status=$?
  trap - EXIT
  set +e
  if (( status != 0 )); then
    for container in "$SERVICE_CONTAINER" "$PROXY_CONTAINER" "$INGESTION_CONTAINER"; do
      if docker inspect "$container" >/dev/null 2>&1; then
        echo "==> Logs from $container" >&2
        docker logs "$container" >&2 || true
      fi
    done
  fi
  docker buildx rm --force "$BUILDER" >/dev/null 2>&1 || true
  for container in "$SERVICE_CONTAINER" "$PROXY_CONTAINER" "$INGESTION_CONTAINER" "$REGISTRY_CONTAINER"; do
    docker rm --force "$container" >/dev/null 2>&1 || true
  done
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  if [[ "$KEEP_WORK" == 1 ]]; then
    echo "E2E work directory retained at $WORK" >&2
  else
    rm -rf "$WORK"
  fi
  exit "$status"
}
trap cleanup EXIT

read_digest() {
  jq -er '."containerimage.digest" | select(type == "string" and test("^sha256:[0-9a-f]{64}$"))' "$1"
}

build_component() {
  local label=$1 target=$2 repository=$3
  shift 3
  local metadata="$WORK/${target}.metadata.json"
  echo "==> Building $label" >&2
  if ! docker buildx build \
    --builder "$BUILDER" \
    --platform "$PLATFORM" \
    --provenance=false \
    --progress="$PROGRESS" \
    --target "$target" \
    --build-arg "SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH" \
    "$@" \
    --tag "$REGISTRY/$repository:$RUN_ID" \
    --metadata-file "$metadata" \
    --push \
    . >&2; then
    return 1
  fi
  local digest
  if ! digest=$(read_digest "$metadata"); then
    echo "component metadata for $target does not contain an image digest:" >&2
    cat "$metadata" >&2 || true
    return 1
  fi
  printf '%s/%s@%s\n' "$REGISTRY" "$repository" "$digest"
}

host_ref() {
  local ref=$1
  printf '%s/%s\n' "$HOST_REGISTRY" "${ref#"${REGISTRY}"/}"
}

wait_for_https() {
  local expected
  for _ in $(seq 1 60); do
    expected=$(curl --cacert "$WORK/tls/ca.pem" --silent --output /dev/null --write-out '%{http_code}' \
      https://localhost:18443/v1/operations/not-an-operation || true)
    if [[ "$expected" == 404 ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

mkdir -p "$WORK/tls" "$WORK/service-store"
inputs="$WORK/release-inputs.json"
./scripts/release-inputs.sh > "$inputs"
SOURCE_DATE_EPOCH=$(jq -er .source_date_epoch "$inputs")
RUNTIME_BASE=$(jq -er '.ubuntu_base["linux/amd64"]' "$inputs")
HOMEBREW_COMMIT=$(jq -er .homebrew_commit "$inputs")

cat > "$WORK/buildkitd.toml" <<EOF_CONFIG
[registry."$REGISTRY"]
  http = true
  insecure = true
EOF_CONFIG

docker network create "$NETWORK" >/dev/null
docker run --detach \
  --name "$REGISTRY_CONTAINER" \
  --network "$NETWORK" \
  --network-alias e2e-registry \
  --publish 127.0.0.1:5000:5000 \
  "$REGISTRY_IMAGE" >/dev/null
for _ in $(seq 1 30); do
  curl --fail --silent http://127.0.0.1:5000/v2/ >/dev/null && break
  sleep 1
done
curl --fail --silent http://127.0.0.1:5000/v2/ >/dev/null

docker buildx create \
  --name "$BUILDER" \
  --driver docker-container \
  --driver-opt "image=$BUILDKIT_IMAGE" \
  --driver-opt "network=$NETWORK" \
  --buildkitd-config "$WORK/buildkitd.toml" \
  --use \
  --bootstrap >/dev/null

RUNTIME_BASE_REF=$(build_component "runtime base" runtime-base dalec-homebrew-runtime-base \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE")
BOTTLE_FETCHER_REF=$(build_component "bottle fetcher" bottle-fetcher dalec-homebrew-bottle-fetcher)
EXTRACTOR_REF=$(build_component "catalog extractor" catalog-extractor dalec-homebrew-catalog-extractor \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE")
SERVICE_REF=$(build_component "catalog service" catalog-service dalec-homebrew-catalog-service)
EXTRACTOR_DIGEST=${EXTRACTOR_REF##*@}
SERVICE_DIGEST=${SERVICE_REF##*@}

openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$WORK/catalog-signing.pem" >/dev/null 2>&1
chmod 0600 "$WORK/catalog-signing.pem"
openssl pkey -in "$WORK/catalog-signing.pem" -pubout -out "$WORK/catalog-public.pem" >/dev/null 2>&1

go run ./cmd/v2-bindings \
  --key-id "$KEY_ID" \
  --public-key "$WORK/catalog-public.pem" \
  --catalog-service-digest "$SERVICE_DIGEST" \
  --catalog-extractor-digest "$EXTRACTOR_DIGEST" \
  --policy-output "$WORK/catalog-key-policy.json" \
  --output "$WORK/v2-bindings.json"

KEY_POLICY_DIGEST=$(jq -er .ingestion_jws_key_policy_digest "$WORK/v2-bindings.json")
KEY_POLICY_BASE64=$(jq -er .ingestion_jws_key_policy_base64 "$WORK/v2-bindings.json")
TAP_POLICY_DIGEST=$(jq -er .tap_policy_digest "$WORK/v2-bindings.json")
RUNTIME_POLICY_DIGEST=$(jq -er .executable_runtime_policy_digest "$WORK/v2-bindings.json")
CATALOG_POLICY_VERSIONS=$(jq -er .supported_catalog_policy_versions "$WORK/v2-bindings.json")
FETCH_POLICY_VERSIONS=$(jq -er .supported_fetch_policy_versions "$WORK/v2-bindings.json")
PROVENANCE_POLICY_VERSIONS=$(jq -er .supported_provenance_policy_versions "$WORK/v2-bindings.json")

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj '/CN=dalec-homebrew-e2e-ca' \
  -keyout "$WORK/tls/ca-key.pem" -out "$WORK/tls/ca.pem" >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes \
  -subj "/CN=$PROXY_CONTAINER" \
  -keyout "$WORK/tls/server-key.pem" -out "$WORK/tls/server.csr" >/dev/null 2>&1
cat > "$WORK/tls/server.ext" <<EOF_CERT
subjectAltName=DNS:$PROXY_CONTAINER,DNS:localhost
extendedKeyUsage=serverAuth
EOF_CERT
openssl x509 -req -days 1 \
  -in "$WORK/tls/server.csr" \
  -CA "$WORK/tls/ca.pem" -CAkey "$WORK/tls/ca-key.pem" -CAcreateserial \
  -extfile "$WORK/tls/server.ext" \
  -out "$WORK/tls/server.pem" >/dev/null 2>&1
chmod 0600 "$WORK/tls/ca-key.pem" "$WORK/tls/server-key.pem"

cat > "$WORK/ingestion-buildkitd.toml" <<EOF_INGESTION
[registry."$REGISTRY"]
  http = true
  insecure = true
EOF_INGESTION

docker run --detach \
  --name "$INGESTION_CONTAINER" \
  --network "$NETWORK" \
  --network-alias catalog-worker \
  --privileged \
  --publish 127.0.0.1:1234:1234 \
  --mount "type=bind,src=$WORK/ingestion-buildkitd.toml,dst=/etc/buildkit/buildkitd.toml,readonly" \
  "$BUILDKIT_IMAGE" \
  --config /etc/buildkit/buildkitd.toml \
  --addr tcp://0.0.0.0:1234 >/dev/null
for _ in $(seq 1 60); do
  if (exec 3<>/dev/tcp/127.0.0.1/1234) 2>/dev/null; then
    exec 3>&-
    break
  fi
  sleep 1
done
(exec 3<>/dev/tcp/127.0.0.1/1234) 2>/dev/null || {
  echo "catalog ingestion BuildKit did not become ready" >&2
  exit 1
}
exec 3>&-

SERVICE_HOST_REF=$(host_ref "$SERVICE_REF")
docker pull "$SERVICE_HOST_REF" >/dev/null
docker run --detach \
  --name "$SERVICE_CONTAINER" \
  --network "$NETWORK" \
  --network-alias catalog-service-http \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,src=$WORK/service-store,dst=/store" \
  --mount "type=bind,src=$WORK/catalog-signing.pem,dst=/run/secrets/catalog-signing.pem,readonly" \
  "$SERVICE_HOST_REF" \
  --listen :8080 \
  --store /store \
  --origin "$CATALOG_ORIGIN" \
  --signing-key /run/secrets/catalog-signing.pem \
  --signing-key-id "$KEY_ID" \
  --buildkit-address tcp://catalog-worker:1234 \
  --extractor-ref "$EXTRACTOR_REF" \
  --homebrew-commit "$HOMEBREW_COMMIT" \
  --service-version e2e \
  --service-digest "$SERVICE_DIGEST" \
  --extractor-version e2e \
  --extractor-digest "$EXTRACTOR_DIGEST" \
  --max-concurrent-generations 1 \
  --max-pending-generations 4 \
  --max-stored-operations 8 >/dev/null

cat > "$WORK/nginx.conf" <<'EOF_NGINX'
events {}
http {
  server {
    listen 443 ssl;
    server_name catalog-service;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_certificate /etc/nginx/tls/server.pem;
    ssl_certificate_key /etc/nginx/tls/server-key.pem;
    client_max_body_size 2m;
    location / {
      proxy_http_version 1.1;
      proxy_set_header Host $host;
      proxy_set_header Connection "";
      proxy_buffering off;
      proxy_pass http://catalog-service-http:8080;
    }
  }
}
EOF_NGINX

docker run --detach \
  --name "$PROXY_CONTAINER" \
  --network "$NETWORK" \
  --publish 127.0.0.1:18443:443 \
  --mount "type=bind,src=$WORK/nginx.conf,dst=/etc/nginx/nginx.conf,readonly" \
  --mount "type=bind,src=$WORK/tls/server.pem,dst=/etc/nginx/tls/server.pem,readonly" \
  --mount "type=bind,src=$WORK/tls/server-key.pem,dst=/etc/nginx/tls/server-key.pem,readonly" \
  "$NGINX_IMAGE" >/dev/null
wait_for_https || {
  echo "catalog HTTPS proxy did not become ready" >&2
  exit 1
}

V2_BUILD_ARGS=(
  --build-arg "BOTTLE_FETCHER_REF=$BOTTLE_FETCHER_REF"
  --build-arg "CATALOG_SERVICE_ORIGIN=$CATALOG_ORIGIN"
  --build-arg "INGESTION_JWS_KEY_POLICY_DIGEST=$KEY_POLICY_DIGEST"
  --build-arg "INGESTION_JWS_KEY_POLICY_BASE64=$KEY_POLICY_BASE64"
  --build-arg "TAP_POLICY_DIGEST=$TAP_POLICY_DIGEST"
  --build-arg "EXECUTABLE_RUNTIME_POLICY_DIGEST=$RUNTIME_POLICY_DIGEST"
  --build-arg "SUPPORTED_CATALOG_POLICY_VERSIONS=$CATALOG_POLICY_VERSIONS"
  --build-arg "SUPPORTED_FETCH_POLICY_VERSIONS=$FETCH_POLICY_VERSIONS"
  --build-arg "SUPPORTED_PROVENANCE_POLICY_VERSIONS=$PROVENANCE_POLICY_VERSIONS"
)

MATERIALIZER_REF=$(build_component materializer materializer dalec-homebrew-materializer \
  --build-arg "RUNTIME_BASE=$RUNTIME_BASE" \
  "${V2_BUILD_ARGS[@]}")
CA_CERT_BASE64=$(base64 < "$WORK/tls/ca.pem" | tr -d '\n')
FRONTEND_REF=$(build_component "V2 frontend" frontend-test-ca dalec-homebrew \
  --build-arg "RUNTIME_BASE_REF=$RUNTIME_BASE_REF" \
  --build-arg "MATERIALIZER_REF=$MATERIALIZER_REF" \
  --build-arg "FRONTEND_TEST_CA_CERTIFICATE_BASE64=$CA_CERT_BASE64" \
  "${V2_BUILD_ARGS[@]}")

DALEC_HOMEBREW_LIVE_BUILDER="$BUILDER" \
DALEC_HOMEBREW_LIVE_PLATFORM="$PLATFORM" \
DALEC_HOMEBREW_LIVE_SPEC="$SPEC" \
DALEC_HOMEBREW_LIVE_RUNTIME_BASE_REF="$RUNTIME_BASE_REF" \
DALEC_HOMEBREW_LIVE_MATERIALIZER_REF="$MATERIALIZER_REF" \
DALEC_HOMEBREW_LIVE_FRONTEND_REF="$FRONTEND_REF" \
DALEC_HOMEBREW_LIVE_SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
DALEC_HOMEBREW_LIVE_IMAGE="$FINAL_IMAGE" \
DALEC_HOMEBREW_LIVE_OUTPUT=load \
DALEC_HOMEBREW_LIVE_PROGRESS="$PROGRESS" \
  ./scripts/live-test.sh

docker run --rm --network none --entrypoint /bin/bash "$FINAL_IMAGE" -lc '
  set -euo pipefail
  hello | grep -F "Hello, world!"
  test -s /home/linuxbrew/.linuxbrew/opt/libdf/lib/libdf.so
  test -s /home/linuxbrew/.linuxbrew/opt/libsrf-proxy-filter/lib/libsrf_filter.so
  grep -F svt/avtools/libdf /usr/share/dalec-homebrew/resolution.json
  grep -F svt/avtools/libsrf-proxy-filter /usr/share/dalec-homebrew/resolution.json
'

cat <<RESULT
DALEC_HOMEBREW_E2E_RUNTIME_BASE_REF=$RUNTIME_BASE_REF
DALEC_HOMEBREW_E2E_BOTTLE_FETCHER_REF=$BOTTLE_FETCHER_REF
DALEC_HOMEBREW_E2E_CATALOG_EXTRACTOR_REF=$EXTRACTOR_REF
DALEC_HOMEBREW_E2E_CATALOG_SERVICE_REF=$SERVICE_REF
DALEC_HOMEBREW_E2E_MATERIALIZER_REF=$MATERIALIZER_REF
DALEC_HOMEBREW_E2E_FRONTEND_REF=$FRONTEND_REF
DALEC_HOMEBREW_E2E_FINAL_IMAGE=$FINAL_IMAGE
RESULT
