#!/usr/bin/env bash
# Runs the Knit Android client's own spool code against this daemon, on the JVM, without a device.
#
# It builds knit-spool, serves it over TLS with a throwaway certificate, mints a relay invite link with
# `commons-invite`, then copies e2e/knit-client/KnitSpoolGoClientTest.kt into a getknit/knit checkout and
# runs it with Knit's unit tests. That test parses the link with Knit's RelayInvite, dials it with
# OkHttpSpoolDialer and talks through SpoolConnection: the code a released app uses.
#
# Needs JDK 21 and an Android SDK (ANDROID_HOME), as the Knit build does. The weekly upstream drift
# workflow runs it against the latest Knit release.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/knit-client-e2e.sh /path/to/getknit-knit-checkout" >&2
  exit 2
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
knit_root=$(cd "$1" && pwd)
test_source="$repo_root/e2e/knit-client/KnitSpoolGoClientTest.kt"
test_target="$knit_root/app/src/test/java/app/getknit/knit/mesh/spool/KnitSpoolGoClientTest.kt"
if [[ ! -d "$(dirname "$test_target")" ]]; then
  echo "$knit_root has no app/src/test/.../mesh/spool; the Knit client layout changed" >&2
  exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/knit-client-e2e.XXXXXX")
chmod 700 "$tmp"
spool_pid=
cleanup() {
  local status=$?
  if [[ -n "$spool_pid" ]]; then
    kill "$spool_pid" 2>/dev/null || true
    wait "$spool_pid" 2>/dev/null || true
  fi
  rm -f "$test_target"
  if [[ $status -ne 0 && -f "$tmp/spool.log" ]]; then
    echo "--- knit-spool log ---" >&2
    tail -n 50 "$tmp/spool.log" >&2
  fi
  rm -rf "$tmp"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

port=${KNIT_CLIENT_E2E_PORT:-29472}
addr="127.0.0.1:$port"
name="Knit client e2e"
token="knit-client-e2e token/+&="

(cd "$repo_root" && CGO_ENABLED=0 go build -trimpath -o "$tmp/knit-spool" ./cmd/knit-spool)

# A self-signed certificate for 127.0.0.1; the test trusts exactly this one.
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 1 \
  -subj "/CN=knit-client-e2e" -addext "subjectAltName=IP:127.0.0.1" \
  -keyout "$tmp/key.pem" -out "$tmp/cert.pem" 2>/dev/null

export SPOOL_PUBLIC_URL="wss://$addr/spool/v1" SPOOL_TOKEN="$token" SPOOL_COMMONS_NAME="$name"
invite=$("$tmp/knit-spool" commons-invite 2>/dev/null)
link=$(sed -n 's/^link: //p' <<<"$invite")
commons_id=$(sed -n 's/^SPOOL_COMMONS_ID=//p' <<<"$invite")
if [[ -z "$link" || -z "$commons_id" ]]; then
  echo "commons-invite printed no link or SPOOL_COMMONS_ID" >&2
  exit 1
fi

SPOOL_LISTEN="$addr" SPOOL_TLS_CERT="$tmp/cert.pem" SPOOL_TLS_KEY="$tmp/key.pem" \
  SPOOL_DATA_DIR="$tmp/data" SPOOL_COMMONS_ID="$commons_id" \
  "$tmp/knit-spool" run >"$tmp/spool.log" 2>&1 &
spool_pid=$!
for _ in $(seq 1 100); do
  if curl -fsS --cacert "$tmp/cert.pem" "https://$addr/healthz" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$spool_pid" 2>/dev/null; then
    echo "knit-spool exited before becoming healthy" >&2
    exit 1
  fi
  sleep 0.1
done
curl -fsS --cacert "$tmp/cert.pem" "https://$addr/healthz" >/dev/null

cp "$test_source" "$test_target"
echo "Knit client: $(git -C "$knit_root" describe --tags --always 2>/dev/null || echo unknown)"
# The test file is ours, so a Kotlin warning in it must not fail Knit's warnings-as-errors build.
(
  cd "$knit_root"
  KNIT_SPOOL_GO_LINK="$link" KNIT_SPOOL_GO_NAME="$name" KNIT_SPOOL_GO_CA="$tmp/cert.pem" \
    ./gradlew :app:testDebugUnitTest \
    --tests 'app.getknit.knit.mesh.spool.KnitSpoolGoClientTest' \
    -Pknit.warningsAsErrors=false --stacktrace --console=plain
)
