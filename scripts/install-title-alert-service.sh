#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
SOURCE_ROOT=$(dirname "$SCRIPT_DIR")
INSTALL_DIR=/opt/seo-weight-alert
SERVICE_NAME=seo-title-alert
SERVICE_USER=seo-weight-alert
SERVICE_GROUP=seo-weight-alert
SERVICE_SOURCE="$SOURCE_ROOT/deploy/$SERVICE_NAME.service"
SERVICE_TARGET="/etc/systemd/system/$SERVICE_NAME.service"

fail() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "run as root"
command -v python3 >/dev/null 2>&1 || fail "python3 is required"
command -v systemctl >/dev/null 2>&1 || fail "systemctl is required"
id "$SERVICE_USER" >/dev/null 2>&1 || fail "$SERVICE_USER does not exist"
[ -f "$SOURCE_ROOT/title_change_notifier.py" ] || fail "$SOURCE_ROOT/title_change_notifier.py does not exist"
[ -f "$SERVICE_SOURCE" ] || fail "$SERVICE_SOURCE does not exist"
[ -f "$INSTALL_DIR/.env" ] || fail "$INSTALL_DIR/.env does not exist"

install -d -o root -g "$SERVICE_GROUP" -m 0750 "$INSTALL_DIR"
install -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 0750 \
  "$SOURCE_ROOT/title_change_notifier.py" "$INSTALL_DIR/title_change_notifier.py"
chown root:"$SERVICE_GROUP" "$INSTALL_DIR/.env"
chmod 0640 "$INSTALL_DIR/.env"
install -m 0644 "$SERVICE_SOURCE" "$SERVICE_TARGET"

systemctl daemon-reload
systemctl enable --now "$SERVICE_NAME.service"

if ! systemctl is-active --quiet "$SERVICE_NAME.service"; then
  journalctl -u "$SERVICE_NAME.service" -n 80 --no-pager >&2 || true
  fail "$SERVICE_NAME.service failed to start"
fi

printf 'Title alert service installed and started.\n'
printf 'Status: systemctl status %s.service\n' "$SERVICE_NAME"
printf 'Logs:   journalctl -u %s.service -f\n' "$SERVICE_NAME"
