#!/usr/bin/env sh
set -eu

umask 027

REPO_URL=${REPO_URL:-https://github.com/userreksai/seo_monitor.git}
BRANCH=${BRANCH:-main}
INSTALL_DIR=/usr/local/seo_monitor
SERVICE_USER=seo-monitor
SERVICE_NAME=seo-monitor
SERVICE_FILE=/etc/systemd/system/${SERVICE_NAME}.service
NOLOGIN_SHELL=$(command -v nologin 2>/dev/null || printf '/usr/sbin/nologin')
APP_MONGODB_URI=${MONGODB_URI:-mongodb://127.0.0.1:27017}
MONGO_ADMIN_URI=${MONGO_ADMIN_URI:-$APP_MONGODB_URI}
SKIP_MONGO_INIT=${SKIP_MONGO_INIT:-0}
DOMAINS_BACKUP=
LEGACY_BACKUP=

log() {
  printf '\n==> %s\n' "$*"
}

fail() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is required but was not found in PATH"
}

git_repo() {
  git -c safe.directory="$INSTALL_DIR" -C "$INSTALL_DIR" "$@"
}

restore_domains() {
  if [ -n "$DOMAINS_BACKUP" ] && [ -f "$DOMAINS_BACKUP" ] && [ -d "$INSTALL_DIR" ]; then
    cp "$DOMAINS_BACKUP" "$INSTALL_DIR/domains.json"
  fi
}

cleanup() {
  restore_domains
  if [ -n "$DOMAINS_BACKUP" ] && [ -f "$DOMAINS_BACKUP" ]; then
    rm -f "$DOMAINS_BACKUP"
  fi
}

generate_token() {
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 32
  else
    od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
  fi
}

trap cleanup EXIT
trap 'exit 1' HUP INT TERM

[ "$(id -u)" -eq 0 ] || fail "run this installer as root: sudo sh install.sh"

require_command git
require_command go
require_command find
require_command systemctl
require_command useradd
if [ "$SKIP_MONGO_INIT" != "1" ]; then
  require_command mongosh
fi

log "Pulling source code into $INSTALL_DIR"
if [ -d "$INSTALL_DIR/.git" ]; then
  if [ -f "$INSTALL_DIR/domains.json" ]; then
    DOMAINS_BACKUP=$(mktemp)
    cp "$INSTALL_DIR/domains.json" "$DOMAINS_BACKUP"
    if git_repo ls-files --error-unmatch domains.json >/dev/null 2>&1; then
      git_repo checkout HEAD -- domains.json
    fi
  fi

  if [ -n "$(git_repo status --porcelain)" ]; then
    git_repo status --short >&2
    fail "repository has local source changes; commit or back them up before installing"
  fi

  git_repo fetch origin "$BRANCH"
  git_repo checkout "$BRANCH"
  git_repo pull --ff-only origin "$BRANCH"
else
  if [ -d "$INSTALL_DIR" ] && [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]; then
    if [ -L "$INSTALL_DIR" ]; then
      fail "$INSTALL_DIR is a symbolic link; refusing to replace it"
    fi
    if [ -f "$INSTALL_DIR/go.mod" ] && grep -Eq '^module[[:space:]]+seo-monitor([[:space:]]|$)' "$INSTALL_DIR/go.mod"; then
      :
    else
      UNEXPECTED_ENTRY=$(find "$INSTALL_DIR" -mindepth 1 -maxdepth 1 ! -name domains.json ! -name .env -print -quit)
      if [ -n "$UNEXPECTED_ENTRY" ]; then
        fail "$INSTALL_DIR contains an unexpected entry: $UNEXPECTED_ENTRY"
      fi
    fi

    log "Migrating existing non-Git configuration or source directory"
    STAGING_DIR=$(mktemp -d "${INSTALL_DIR}.new.XXXXXX")
    git clone --branch "$BRANCH" --single-branch "$REPO_URL" "$STAGING_DIR/repository"

    if [ -f "$INSTALL_DIR/.env" ]; then
      cp -p "$INSTALL_DIR/.env" "$STAGING_DIR/repository/.env"
    fi
    if [ -f "$INSTALL_DIR/domains.json" ]; then
      cp -p "$INSTALL_DIR/domains.json" "$STAGING_DIR/repository/domains.json"
    fi

    LEGACY_BACKUP="${INSTALL_DIR}.backup.$(date '+%Y%m%d%H%M%S')"
    mv "$INSTALL_DIR" "$LEGACY_BACKUP"
    if ! mv "$STAGING_DIR/repository" "$INSTALL_DIR"; then
      mv "$LEGACY_BACKUP" "$INSTALL_DIR"
      fail "failed to activate cloned repository; the original directory was restored"
    fi
    rmdir "$STAGING_DIR" 2>/dev/null || true
    log "Original source directory preserved at $LEGACY_BACKUP"
  else
    mkdir -p "$INSTALL_DIR"
    git clone --branch "$BRANCH" --single-branch "$REPO_URL" "$INSTALL_DIR"
  fi
fi

restore_domains
if [ ! -f "$INSTALL_DIR/domains.json" ]; then
  cp "$INSTALL_DIR/domains.example.json" "$INSTALL_DIR/domains.json"
fi

log "Creating service account"
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home "$INSTALL_DIR" --shell "$NOLOGIN_SHELL" "$SERVICE_USER"
fi

log "Preparing application environment"
if [ ! -f "$INSTALL_DIR/.env" ]; then
  APP_API_TOKEN=${API_TOKEN:-$(generate_token)}
  {
    printf 'MONGODB_URI=%s\n' "$APP_MONGODB_URI"
    printf 'MONGODB_DATABASE=seo_monitor\n'
    printf 'ENSURE_INDEXES=true\n'
    printf 'DOMAINS_FILE=domains.json\n'
    printf 'HTTP_ADDR=127.0.0.1:10001\n'
    printf 'API_TOKEN=%s\n' "$APP_API_TOKEN"
    printf 'CORS_ALLOWED_ORIGINS=\n'
    printf 'SOURCE_BASE_URL=https://seo.chinaz.com\n'
    printf 'SOURCE_DATA_URL=https://othertool.chinaz.com\n'
    printf 'SCRAPE_USER_AGENT="seo-monitor/1.0 (daily metrics collector; contact your administrator)"\n'
    printf 'SCRAPE_TIMEOUT=25s\n'
    printf 'SCRAPE_MIN_DELAY=3s\n'
    printf 'SCRAPE_MAX_DELAY=8s\n'
    printf 'SCRAPE_RETRIES=3\n'
    printf 'MAX_RESPONSE_BYTES=3145728\n'
    printf 'WORKER_COUNT=1\n'
    printf 'JOB_POLL_INTERVAL=2s\n'
    printf 'STALE_JOB_AFTER=20m\n'
    printf 'RETENTION_DAYS=60\n'
    printf 'SNAPSHOT_TIMEZONE=Asia/Shanghai\n'
    printf 'COLLECT_CRON="15 2 * * *"\n'
    printf 'QUEUE_ON_START=true\n'
    printf 'CERTIFICATE_WORKERS=10\n'
    printf 'CERTIFICATE_TIMEOUT=8s\n'
    printf 'CERTIFICATE_CRON="45 3 * * *"\n'
    printf 'CERTIFICATE_AGENT_URLS=\n'
    printf 'CERTIFICATE_AGENT_TOKEN=\n'
    printf 'CERTIFICATE_AGENT_TIMEOUT=15s\n'
    printf 'CERTIFICATE_AGENT_MAX_CONCURRENT=4\n'
  } >"$INSTALL_DIR/.env"
  log "Created $INSTALL_DIR/.env with a generated API token"
else
  ENV_TEMP=$(mktemp "$INSTALL_DIR/.env.XXXXXX")
  awk '
    BEGIN {
      http_updated = 0
      retention_seen = 0
      certificate_agent_urls_seen = 0
      certificate_agent_token_seen = 0
      certificate_agent_timeout_seen = 0
      certificate_agent_concurrency_seen = 0
    }
    /^HTTP_ADDR=/ {
      if (!http_updated) {
        print "HTTP_ADDR=127.0.0.1:10001"
        http_updated = 1
      }
      next
    }
    /^RETENTION_DAYS=/ { retention_seen = 1 }
    /^CERTIFICATE_AGENT_URLS=/ { certificate_agent_urls_seen = 1 }
    /^CERTIFICATE_AGENT_TOKEN=/ { certificate_agent_token_seen = 1 }
    /^CERTIFICATE_AGENT_TIMEOUT=/ { certificate_agent_timeout_seen = 1 }
    /^CERTIFICATE_AGENT_MAX_CONCURRENT=/ { certificate_agent_concurrency_seen = 1 }
    { print }
    END {
      if (!http_updated) print "HTTP_ADDR=127.0.0.1:10001"
      if (!retention_seen) print "RETENTION_DAYS=60"
      if (!certificate_agent_urls_seen) print "CERTIFICATE_AGENT_URLS="
      if (!certificate_agent_token_seen) print "CERTIFICATE_AGENT_TOKEN="
      if (!certificate_agent_timeout_seen) print "CERTIFICATE_AGENT_TIMEOUT=15s"
      if (!certificate_agent_concurrency_seen) print "CERTIFICATE_AGENT_MAX_CONCURRENT=4"
    }
  ' "$INSTALL_DIR/.env" >"$ENV_TEMP"
  mv "$ENV_TEMP" "$INSTALL_DIR/.env"
  log "Keeping existing $INSTALL_DIR/.env and ensuring required defaults are configured"
fi
chmod 600 "$INSTALL_DIR/.env"

if [ "$SKIP_MONGO_INIT" = "1" ]; then
  log "Skipping MongoDB initialization because SKIP_MONGO_INIT=1"
else
  log "Initializing MongoDB database seo_monitor"
  mongosh "$MONGO_ADMIN_URI" --quiet --file "$INSTALL_DIR/scripts/mongo-init.js"
fi

log "Testing and building the Go service"
sh "$INSTALL_DIR/build.sh"

log "Installing systemd service"
cp "$INSTALL_DIR/deploy/seo-monitor.service" "$SERVICE_FILE"
chmod 644 "$SERVICE_FILE"
chown -R "$SERVICE_USER:$SERVICE_USER" "$INSTALL_DIR"

systemctl daemon-reload
systemctl enable "$SERVICE_NAME.service" >/dev/null
systemctl restart "$SERVICE_NAME.service"

if ! systemctl is-active --quiet "$SERVICE_NAME.service"; then
  journalctl -u "$SERVICE_NAME.service" -n 50 --no-pager >&2 || true
  fail "$SERVICE_NAME.service failed to start"
fi

if command -v curl >/dev/null 2>&1; then
  attempt=0
  healthy=0
  while [ "$attempt" -lt 20 ]; do
    if curl -fsS --max-time 2 http://127.0.0.1:10001/healthz >/dev/null 2>&1; then
      healthy=1
      break
    fi
    attempt=$((attempt + 1))
    sleep 1
  done
  if [ "$healthy" -ne 1 ]; then
    journalctl -u "$SERVICE_NAME.service" -n 50 --no-pager >&2 || true
    fail "service is active but the health endpoint did not become ready"
  fi
fi

log "Installation complete"
printf 'Source:  %s\n' "$INSTALL_DIR"
printf 'Domains: %s/domains.json\n' "$INSTALL_DIR"
printf 'Config:  %s/.env\n' "$INSTALL_DIR"
if [ -n "$LEGACY_BACKUP" ]; then
  printf 'Backup:  %s\n' "$LEGACY_BACKUP"
fi
printf 'Service: systemctl status %s\n' "$SERVICE_NAME"
printf 'Logs:    journalctl -u %s -f\n' "$SERVICE_NAME"
