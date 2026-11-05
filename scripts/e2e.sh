#!/usr/bin/env bash
# Runs the end-to-end test against a throwaway Synapse in Docker.
set -euo pipefail

cd "$(dirname "$0")/.."

NAME=matrix-mirror-e2e-synapse
PORT=${MM_E2E_PORT:-18008}
IMAGE=${MM_E2E_IMAGE:-matrixdotorg/synapse:latest}
SECRET=e2e-registration-secret
DATA=$(mktemp -d)

cleanup() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    rm -rf "$DATA"
}
trap cleanup EXIT

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run --rm -v "$DATA:/data" -e SYNAPSE_SERVER_NAME=localhost -e SYNAPSE_REPORT_STATS=no "$IMAGE" generate >/dev/null

grep -v '^registration_shared_secret:' "$DATA/homeserver.yaml" > "$DATA/homeserver.yaml.new"
mv "$DATA/homeserver.yaml.new" "$DATA/homeserver.yaml"
cat >> "$DATA/homeserver.yaml" <<EOF
registration_shared_secret: "$SECRET"
rc_message: {per_second: 1000, burst_count: 1000}
rc_login:
  address: {per_second: 1000, burst_count: 1000}
  account: {per_second: 1000, burst_count: 1000}
  failed_attempts: {per_second: 1000, burst_count: 1000}
rc_joins:
  local: {per_second: 1000, burst_count: 1000}
rc_invites:
  per_room: {per_second: 1000, burst_count: 1000}
  per_user: {per_second: 1000, burst_count: 1000}
rc_key_requests: {per_second: 1000, burst_count: 1000}
EOF

docker run -d --name "$NAME" -p "127.0.0.1:$PORT:8008" -v "$DATA:/data" "$IMAGE" >/dev/null

for _ in $(seq 1 60); do
    if curl -sf "http://127.0.0.1:$PORT/_matrix/client/versions" >/dev/null; then
        break
    fi
    sleep 1
done

MM_E2E_HOMESERVER="http://127.0.0.1:$PORT" MM_E2E_SECRET="$SECRET" \
    CGO_ENABLED=0 go test -tags goolm,e2e -count=1 -timeout 10m ./e2e/... -v "$@"
