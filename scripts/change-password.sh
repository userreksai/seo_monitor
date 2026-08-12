#!/usr/bin/env sh
set -eu

umask 077
export LC_ALL=C

APP_DIR=${SEO_MONITOR_DIR:-/usr/local/seo_monitor}
ENV_FILE=${SEO_MONITOR_ENV_FILE:-$APP_DIR/.env}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required but was not found in PATH"
}

read_env_value() {
  key=$1
  [ -r "$ENV_FILE" ] || return 0
  awk -v key="$key" 'index($0, key "=") == 1 { print substr($0, length(key) + 2); exit }' "$ENV_FILE"
}

ALLOW_WEAK_PASSWORD=0
USERNAME=
REQUESTED_ROLE=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-weak-password)
      ALLOW_WEAK_PASSWORD=1
      ;;
    -h|--help)
      echo "usage: sudo sh $0 [--allow-weak-password] [username] [admin|readonly]"
      exit 0
      ;;
    -* )
      fail "unknown option: $1"
      ;;
    *)
      if [ -z "$USERNAME" ]; then
        USERNAME=$1
      elif [ -z "$REQUESTED_ROLE" ]; then
        REQUESTED_ROLE=$1
      else
        fail "too many arguments"
      fi
      ;;
  esac
  shift
done

USERNAME=${USERNAME:-admin}
[ -n "$USERNAME" ] || fail "username cannot be empty"
case "$REQUESTED_ROLE" in
  ''|admin|readonly) ;;
  *) fail "role must be admin or readonly" ;;
esac

MONGO_URI_FROM_FILE=$(read_env_value MONGODB_URI)
MONGO_DATABASE_FROM_FILE=$(read_env_value MONGODB_DATABASE)
MONGO_URI=${MONGODB_URI:-${MONGO_URI_FROM_FILE:-mongodb://127.0.0.1:27017}}
MONGO_DATABASE=${MONGODB_DATABASE:-${MONGO_DATABASE_FROM_FILE:-seo_monitor}}

require_command mongosh
[ -r /dev/tty ] || fail "an interactive terminal is required"

restore_terminal() {
  stty echo < /dev/tty 2>/dev/null || true
}

trap restore_terminal EXIT
trap 'restore_terminal; exit 1' HUP INT TERM
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

[ "$NEW_PASSWORD" = "$CONFIRM_PASSWORD" ] || fail "the two passwords do not match"
unset CONFIRM_PASSWORD

PASSWORD_BYTES=$(printf '%s' "$NEW_PASSWORD" | wc -c | tr -d '[:space:]')
MIN_PASSWORD_BYTES=12
if [ "$ALLOW_WEAK_PASSWORD" -eq 1 ]; then
  MIN_PASSWORD_BYTES=8
  echo "WARNING: allowing an 8-11 byte password; this is unsafe for a public account." >&2
fi
[ "$PASSWORD_BYTES" -ge "$MIN_PASSWORD_BYTES" ] || fail "password must contain at least $MIN_PASSWORD_BYTES bytes"
[ "$PASSWORD_BYTES" -le 72 ] || fail "password must contain at most 72 bytes"

if command -v htpasswd >/dev/null 2>&1; then
  HASH_LINE=$(printf '%s\n' "$NEW_PASSWORD" | htpasswd -niBC 10 seo-monitor)
  PASSWORD_HASH=${HASH_LINE#*:}
elif command -v python3 >/dev/null 2>&1 && python3 -c 'import bcrypt' >/dev/null 2>&1; then
  PASSWORD_HASH=$(printf '%s\n' "$NEW_PASSWORD" | python3 -c '
import bcrypt
import sys

password = sys.stdin.buffer.read()
if password.endswith(b"\n"):
    password = password[:-1]
sys.stdout.write(bcrypt.hashpw(password, bcrypt.gensalt(rounds=10)).decode("ascii"))
')
else
  fail "a bcrypt generator is required; install it with: apt-get install -y apache2-utils"
fi
unset NEW_PASSWORD

case "$PASSWORD_HASH" in
  '$2a$'*|'$2b$'*|'$2y$'*) ;;
  *) fail "failed to generate a compatible bcrypt password hash" ;;
esac

SEO_MONITOR_USERNAME=$USERNAME \
SEO_MONITOR_PASSWORD_HASH=$PASSWORD_HASH \
SEO_MONITOR_DATABASE=$MONGO_DATABASE \
SEO_MONITOR_ROLE=$REQUESTED_ROLE \
mongosh "$MONGO_URI" --quiet --eval '
const databaseName = process.env.SEO_MONITOR_DATABASE;
const username = process.env.SEO_MONITOR_USERNAME.trim().toLowerCase();
const passwordHash = process.env.SEO_MONITOR_PASSWORD_HASH;
const requestedRole = process.env.SEO_MONITOR_ROLE;

if (!databaseName || !/^[A-Za-z0-9_.-]+$/.test(databaseName)) {
  throw new Error("invalid MongoDB database name");
}
if (!username || username.length > 100 || /[\u0000-\u001f\u007f]/.test(username)) {
  throw new Error("username must contain 1 to 100 characters and no control characters");
}
if (!/^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$/.test(passwordHash)) {
  throw new Error("invalid bcrypt password hash");
}
if (requestedRole && requestedRole !== "admin" && requestedRole !== "readonly") {
  throw new Error("role must be admin or readonly");
}

const database = db.getSiblingDB(databaseName);
const users = database.getCollection("users");
users.createIndex({ username: 1 }, { name: "uq_users_username", unique: true });

const existingUser = users.findOne({ username: username }, { _id: 1, role: 1 });
const existed = existingUser !== null;
const role = requestedRole || (existingUser && existingUser.role) || "admin";
const now = new Date();
const result = users.updateOne(
  { username: username },
  {
    $set: {
      password_hash: passwordHash,
      password_changed_at: now,
      role: role,
      active: true,
      updated_at: now
    },
    $setOnInsert: {
      created_at: now
    }
  },
  { upsert: true }
);
if (!result.acknowledged) {
  throw new Error("MongoDB did not acknowledge the user update");
}

const user = users.findOne({ username: username }, { _id: 1 });
if (!user) {
  throw new Error("user was not found after update");
}
const revoked = database.getCollection("auth_sessions").deleteMany({ user_id: user._id });
print((existed ? "Updated" : "Created") + " account: " + username);
print("Role: " + role);
print("Revoked sessions: " + revoked.deletedCount);
'

unset PASSWORD_HASH
