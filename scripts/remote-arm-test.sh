#!/usr/bin/env bash
# Runs this repository's Go tests and the end-to-end test on a real 32-bit ARM Linux host, such as a
# Raspberry Pi, without installing Go there.
#
# It cross-compiles the daemon and the test binaries here (GOARCH=arm, GOARM=7 by default), copies them
# with the source tree into a fresh temporary directory on the host over SSH, runs them, and removes the
# directory. The e2e test starts its own daemon on a loopback port with a throwaway store and
# certificate, so a spool already running on the host is not touched. Nothing needs root.
#
#   scripts/remote-arm-test.sh pi2
#   GOARM=6 scripts/remote-arm-test.sh pi-zero
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/remote-arm-test.sh SSH_HOST" >&2
  exit 2
fi
host=$1
goarm=${GOARM:-7}

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"
local_tmp=$(mktemp -d "${TMPDIR:-/tmp}/remote-arm-test.XXXXXX")
trap 'rm -rf "$local_tmp"' EXIT

stage="$local_tmp/stage"
mkdir -p "$stage/bin"
# The root package's tests read README.md, knit-reference.lock and the workflows, so ship the tree.
git ls-files -z | COPYFILE_DISABLE=1 tar --no-xattrs --null -T - -cf - | tar -xf - -C "$stage"

export CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM="$goarm"
go build -trimpath -o "$stage/bin/knit-spool" ./cmd/knit-spool
go test -c -o "$stage/bin/spool.test" .
go test -c -o "$stage/bin/cmd.test" ./cmd/knit-spool
go test -c -o "$stage/bin/e2e.test" ./e2e
echo "built for linux/arm GOARM=$goarm from $(git rev-parse --short HEAD)$(git diff --quiet HEAD || echo '+dirty')"

# The archive goes over stdin, so the remote script travels as the SSH command (run by the login shell,
# bash on Raspberry Pi OS).
remote_script=$(cat <<'REMOTE'
set -euo pipefail
dir=$(mktemp -d "${TMPDIR:-/tmp}/knit-spool-arm-test.XXXXXX")
trap 'rm -rf "$dir"' EXIT
tar -xzf - -C "$dir"
echo "host: $(uname -srm), $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release | tr -d '"')"
status=0
run() {
  local name=$1 workdir=$2
  shift 2
  echo "== $name"
  if ! (cd "$dir/$workdir" && "$@"); then
    status=1
  fi
}
run "spool package" . ./bin/spool.test -test.count=1
run "cmd/knit-spool" cmd/knit-spool ../../bin/cmd.test -test.count=1
run "e2e (relay invite link against the running binary)" e2e \
  env KNIT_SPOOL_BINARY=../bin/knit-spool ../bin/e2e.test -test.count=1 -test.v
exit "$status"
REMOTE
)
COPYFILE_DISABLE=1 tar --no-xattrs -C "$stage" -czf - . | ssh "$host" "$remote_script"
