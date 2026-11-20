#!/usr/bin/env bash
# Installs knit-spool-update and enables its daily timer. Run as root from a checkout:
#   sudo deploy/update/install.sh
# Then write /etc/knit-spool/update.env (see update.env.example) and run the first update with
#   sudo systemctl start knit-spool-update && journalctl -u knit-spool-update -n 50
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
install -D -o root -g root -m 0755 "$here/knit-spool-update" /usr/local/libexec/knit-spool-update
install -o root -g root -m 0644 "$here/knit-spool-update.service" "$here/knit-spool-update.timer" \
  /etc/systemd/system/
install -d -o root -g root -m 0700 /var/backups/knit-spool
if [[ ! -e /etc/knit-spool/update.env ]]; then
  echo "write /etc/knit-spool/update.env before the first run (see $here/update.env.example)" >&2
fi
systemctl daemon-reload
systemctl enable --now knit-spool-update.timer
systemctl list-timers knit-spool-update.timer --no-pager
