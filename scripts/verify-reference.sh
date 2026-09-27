#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/verify-reference.sh /path/to/getknit-spool-checkout" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"
reference_root=$(cd "$1" && pwd)
expected_reference=0c51be517db5a0ff90587c6e5045bdd019fe4b40
actual_reference=$(git -C "$reference_root" rev-parse HEAD)
if [[ "$actual_reference" != "$expected_reference" ]]; then
  echo "reference checkout must be $expected_reference (found $actual_reference)" >&2
  exit 2
fi

if [[ -z "${JAVA_HOME:-}" ]]; then
  echo "set JAVA_HOME to JDK 21" >&2
  exit 2
fi
java_version=$("$JAVA_HOME/bin/java" -version 2>&1 | head -n 1)
if [[ "$java_version" != *'version "21'* ]]; then
  echo "JDK 21 is required by the Kotlin reference build (JAVA_HOME currently reports: $java_version)" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/knit-spool-reference.XXXXXX")
chmod 700 "$tmp"
go_pid=
reference_pid=
cleanup() {
  local status=$?
  if [[ -n "$go_pid" ]]; then
    kill "$go_pid" 2>/dev/null || true
    wait "$go_pid" 2>/dev/null || true
  fi
  if [[ -n "$reference_pid" ]]; then
    kill "$reference_pid" 2>/dev/null || true
    wait "$reference_pid" 2>/dev/null || true
  fi
  if [[ $status -ne 0 ]]; then
    for log in "$tmp"/*.log; do
      [[ -f "$log" ]] || continue
      echo "--- $(basename "$log") ---" >&2
      tail -n 50 "$log" >&2
    done
  fi
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

go_port=${SPOOL_CHECK_GO_PORT:-29470}
reference_port=${SPOOL_CHECK_REFERENCE_PORT:-29471}
conformance_token=knit-spool-ci-fixture-token
for port in "$go_port" "$reference_port"; do
  if ! python3 - "$port" <<'PY'
import socket, sys
sock = socket.socket()
try:
    sock.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
  then
    echo "loopback port $port is already in use" >&2
    exit 2
  fi
done

python3 - "$tmp/commons-values" <<'PY'
import base64, hashlib, pathlib, sys
secret = bytes([42]) * 32
invite = "knit-commons:v1:" + base64.urlsafe_b64encode(secret).decode().rstrip("=")
scope_id = hashlib.sha256(b"knit/spool/v1/commons" + secret).hexdigest()
pathlib.Path(sys.argv[1]).write_text(invite + "\t" + scope_id + "\n")
PY
chmod 600 "$tmp/commons-values"
IFS=$'\t' read -r commons_invite commons_id < "$tmp/commons-values"

export PATH="$JAVA_HOME/bin:$PATH"
(cd "$reference_root" && ./gradlew :daemon:installDist :conformance:installDist) >"$tmp/gradle.log" 2>&1 || {
  tail -n 60 "$tmp/gradle.log" >&2
  exit 1
}

go_binary="$tmp/knit-spool"
GOTOOLCHAIN=local go build -trimpath -o "$go_binary" ./cmd/knit-spool

env \
  SPOOL_LISTEN="127.0.0.1:$go_port" \
  SPOOL_DATA_PATH="$tmp/go/spool.db" \
  SPOOL_POW_BITS=8 \
  SPOOL_MAX_SCOPES=256 \
  SPOOL_RATE_RECORDS=1000 \
  SPOOL_RATE_PUSHES=10000 \
  SPOOL_RATE_NEW_SCOPES=1000 \
  SPOOL_TOKEN="$conformance_token" \
  SPOOL_COMMONS_ID="$commons_id" \
  SPOOL_COMMONS_NAME="Conformance fixture" \
  SPOOL_COMMONS_MAX_FRAMES=500 \
  "$go_binary" run >"$tmp/go.log" 2>&1 &
go_pid=$!

env \
  SPOOL_PORT="$reference_port" \
  SPOOL_DATA_DIR="$tmp/reference" \
  SPOOL_POW_BITS=8 \
  SPOOL_MAX_SCOPES=256 \
  SPOOL_RATE_RECORDS=1000 \
  SPOOL_RATE_PUSHES=10000 \
  SPOOL_RATE_NEW_SCOPES=1000 \
  SPOOL_TOKEN="$conformance_token" \
  SPOOL_COMMONS_ID="$commons_id" \
  SPOOL_COMMONS_NAME="Conformance fixture" \
  SPOOL_COMMONS_MAX_FRAMES=500 \
  "$reference_root/daemon/build/install/knit-spool/bin/knit-spool" >"$tmp/reference.log" 2>&1 &
reference_pid=$!

wait_ready() {
  local port=$1 pid=$2 name=$3
  for _ in $(seq 1 120); do
    if curl --silent --fail "http://127.0.0.1:$port/healthz" >/dev/null; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "$name exited before readiness" >&2
      return 1
    fi
    sleep 0.25
  done
  echo "$name did not become healthy" >&2
  return 1
}
wait_ready "$go_port" "$go_pid" "Go spool"
wait_ready "$reference_port" "$reference_pid" "Kotlin reference spool"

conformance="$reference_root/conformance/build/install/knit-spool-conformance/bin/knit-spool-conformance"
KNIT_LOCAL_URL="ws://127.0.0.1:$go_port/spool/v1?k=$conformance_token" \
KNIT_REFERENCE_URL="ws://127.0.0.1:$reference_port/spool/v1?k=$conformance_token" \
  go test -count=1 -run '^TestDifferentialAgainstOfficialKotlinReference$' .

for target in "Go:$go_port" "Kotlin-reference:$reference_port"; do
  name=${target%%:*}
  port=${target##*:}
  log="$tmp/conformance-$name.log"
  if ! "$conformance" "ws://127.0.0.1:$port/spool/v1" --token "$conformance_token" --timeout-ms 10000 --pow-limit 24 --destructive --commons-invite "$commons_invite" >"$log" 2>&1; then
    echo "official conformance failed for $name" >&2
    cat "$log" >&2
    exit 1
  fi
  echo "Official conformance passed: $name"
  tail -n 4 "$log"
done

echo "Reference checkout: $actual_reference"
