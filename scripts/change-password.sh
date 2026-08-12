#!/usr/bin/env sh
set -eu

umask 077

fail() {
  echo "error: $*" >&2
  exit 1
}

if [ "$(id -u)" -ne 0 ]; then
  fail "please run this script as root (for example: sudo sh $0)"
fi

ALLOW_WEAK_PASSWORD=0
USERNAME=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-weak-password)
      ALLOW_WEAK_PASSWORD=1
      ;;
    -h|--help)
      echo "usage: sudo sh $0 [--allow-weak-password] [username]"
      exit 0
      ;;
    -*)
      fail "unknown option: $1"
      ;;
    *)
      [ -z "$USERNAME" ] || fail "only one username may be specified"
      USERNAME=$1
      ;;
  esac
  shift
done

USERNAME=${USERNAME:-admin}
[ -n "$USERNAME" ] || fail "username cannot be empty"

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
APP_DIR=${SEO_MONITOR_DIR:-$(dirname -- "$SCRIPT_DIR")}
ENV_FILE=${SEO_MONITOR_ENV_FILE:-$APP_DIR/.env}
HELPER=${SEO_MONITOR_PASSWORD_HELPER:-$APP_DIR/bin/seo-monitor-change-password}

[ -r "$ENV_FILE" ] || fail "cannot read environment file: $ENV_FILE"
[ -x "$HELPER" ] || fail "password helper is missing; rerun the backend installer: $HELPER"
[ -r /dev/tty ] || fail "an interactive terminal is required"

restore_terminal() {
  stty echo < /dev/tty 2>/dev/null || true
}

trap restore_terminal EXIT
trap 'restore_terminal; exit 1' HUP INT TERM
if [ "$ALLOW_WEAK_PASSWORD" -eq 1 ]; then
  echo "WARNING: allowing an 8-11 byte password; this is unsafe for a public account." >&2
fi
printf 'New password for %s: ' "$USERNAME" > /dev/tty
stty -echo < /dev/tty
if ! IFS= read -r NEW_PASSWORD < /dev/tty; then
  fail "could not read the new password"
fi
printf '\nConfirm new password: ' > /dev/tty
if ! IFS= read -r CONFIRM_PASSWORD < /dev/tty; then
  fail "could not read the password confirmation"
fi
restore_terminal
trap - EXIT HUP INT TERM
printf '\n' > /dev/tty

[ -n "$NEW_PASSWORD" ] || fail "password cannot be empty"
[ "$NEW_PASSWORD" = "$CONFIRM_PASSWORD" ] || fail "the two passwords do not match"
unset CONFIRM_PASSWORD

set -a
# The deployment environment file is root-controlled and uses shell-compatible values.
. "$ENV_FILE"
set +a

if [ "$ALLOW_WEAK_PASSWORD" -eq 1 ]; then
  printf '%s\n' "$NEW_PASSWORD" | "$HELPER" -username "$USERNAME" -allow-weak-password
else
  printf '%s\n' "$NEW_PASSWORD" | "$HELPER" -username "$USERNAME"
fi
unset NEW_PASSWORD
