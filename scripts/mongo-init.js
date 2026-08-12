// Run with:
// mongosh "mongodb://USER:PASSWORD@HOST:27017/admin" --file scripts/mongo-init.js
// MongoDB creates the new database lazily when the first collection is created.
db = db.getSiblingDB("seo_monitor");

function ensureCollection(name, validator) {
  const options = {
    validator: { $jsonSchema: validator },
    validationLevel: "strict",
    validationAction: "error",
  };
  if (!db.getCollectionNames().includes(name)) {
    db.createCollection(name, options);
  } else {
    db.runCommand({ collMod: name, ...options });
  }
}

ensureCollection("domains", {
  bsonType: "object",
  title: "Monitored domain",
  required: ["domain", "active", "created_at", "updated_at"],
  properties: {
    _id: { bsonType: "objectId" },
    domain: { bsonType: "string", description: "小写、去协议后的域名" },
    display_name: { bsonType: "string" },
    active: { bsonType: "bool" },
    certificate_active: { bsonType: "bool" },
    created_at: { bsonType: "date" },
    updated_at: { bsonType: "date" },
    archived_at: { bsonType: "date" },
  },
});

ensureCollection("domain_daily_metrics", {
  bsonType: "object",
  title: "Daily SEO metric snapshot",
  required: ["domain_id", "domain", "snapshot_date", "collected_at", "source_url", "raw_sha256"],
  properties: {
    _id: { bsonType: "objectId" },
    domain_id: { bsonType: "objectId" },
    domain: { bsonType: "string" },
    snapshot_date: { bsonType: "date", description: "采集时区当天 00:00:00 对应的 UTC 时间" },
    collected_at: { bsonType: "date" },
    traffic_text: { bsonType: "string" },
    traffic_min: { bsonType: ["int", "long"] },
    traffic_max: { bsonType: ["int", "long"] },
    baidu_pc_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    baidu_mobile_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    sogou_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    bing_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    so_360_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    shenma_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    pr_weight: { bsonType: "int", minimum: 0, maximum: 10 },
    apppc_pc_rank: { bsonType: ["int", "long"] },
    site_category: { bsonType: "string" },
    backlink_count: { bsonType: ["int", "long"] },
    registrant_name: { bsonType: "string" },
    registrant_email: { bsonType: "string" },
    domain_age_text: { bsonType: "string" },
    domain_age_days: { bsonType: "int" },
    expires_on: { bsonType: "date" },
    source_url: { bsonType: "string" },
    raw_sha256: { bsonType: "string", pattern: "^[a-f0-9]{64}$" },
  },
});

ensureCollection("collection_jobs", {
  bsonType: "object",
  title: "Durable collection queue",
  required: ["domain_id", "domain", "snapshot_date", "status", "requested_by", "attempt_count", "queued_at"],
  properties: {
    _id: { bsonType: "objectId" },
    domain_id: { bsonType: "objectId" },
    domain: { bsonType: "string" },
    snapshot_date: { bsonType: "date" },
    status: { enum: ["queued", "running", "succeeded", "failed", "canceled"] },
    requested_by: { enum: ["startup", "scheduler", "manual", "manual-force"] },
    attempt_count: { bsonType: "int", minimum: 0 },
    queued_at: { bsonType: "date" },
    started_at: { bsonType: "date" },
    finished_at: { bsonType: "date" },
    error_message: { bsonType: "string" },
    dedupe_key: { bsonType: "string" },
  },
});

ensureCollection("domain_certificates", {
  bsonType: "object",
  title: "Latest domain TLS certificate",
  required: ["domain_id", "domain", "checked_at", "hostname_valid"],
  properties: {
    _id: { bsonType: "objectId" },
    domain_id: { bsonType: "objectId" },
    domain: { bsonType: "string" },
    issuer: { bsonType: "string" },
    subject: { bsonType: "string" },
    serial_number: { bsonType: "string" },
    dns_names: { bsonType: "array", items: { bsonType: "string" } },
    valid_from: { bsonType: "date" },
    expires_at: { bsonType: "date" },
    check_date: { bsonType: "date" },
    checked_at: { bsonType: "date" },
    hostname_valid: { bsonType: "bool" },
    check_source: { bsonType: "string" },
    resolved_address: { bsonType: "string" },
    error_message: { bsonType: "string" },
  },
});

ensureCollection("domain_certificate_history", {
  bsonType: "object",
  title: "Daily TLS certificate polling history",
  required: ["domain_id", "domain", "check_date", "checked_at", "hostname_valid"],
  properties: {
    _id: { bsonType: "objectId" },
    domain_id: { bsonType: "objectId" },
    domain: { bsonType: "string" },
    issuer: { bsonType: "string" },
    subject: { bsonType: "string" },
    serial_number: { bsonType: "string" },
    dns_names: { bsonType: "array", items: { bsonType: "string" } },
    valid_from: { bsonType: "date" },
    expires_at: { bsonType: "date" },
    check_date: { bsonType: "date" },
    checked_at: { bsonType: "date" },
    hostname_valid: { bsonType: "bool" },
    check_source: { bsonType: "string" },
    resolved_address: { bsonType: "string" },
    error_message: { bsonType: "string" },
  },
});

ensureCollection("users", {
  bsonType: "object",
  title: "Application user",
  required: ["username", "password_hash", "role", "active", "created_at", "updated_at"],
  properties: {
    _id: { bsonType: "objectId" },
    username: { bsonType: "string", minLength: 1, maxLength: 100 },
    password_hash: { bsonType: "string", description: "bcrypt password hash" },
    role: { bsonType: "string" },
    active: { bsonType: "bool" },
    created_at: { bsonType: "date" },
    updated_at: { bsonType: "date" },
    last_login_at: { bsonType: "date" },
  },
});

ensureCollection("auth_sessions", {
  bsonType: "object",
  title: "Application login session",
  required: ["user_id", "token_hash", "created_at", "expires_at"],
  properties: {
    _id: { bsonType: "objectId" },
    user_id: { bsonType: "objectId" },
    token_hash: { bsonType: "string", pattern: "^[a-f0-9]{64}$" },
    created_at: { bsonType: "date" },
    expires_at: { bsonType: "date" },
  },
});

db.domains.createIndex(
  { domain: 1 },
  { name: "uq_domains_domain", unique: true }
);
db.domains.createIndex(
  { active: 1, created_at: -1 },
  { name: "ix_domains_active_created" }
);
db.domains.createIndex(
  { certificate_active: 1, created_at: -1 },
  { name: "ix_domains_certificate_active_created" }
);

// 核心唯一值：同一域名在同一个采集日只能有一份快照。
db.domain_daily_metrics.createIndex(
  { domain: 1, snapshot_date: 1 },
  { name: "uq_metrics_domain_date", unique: true }
);
db.domain_daily_metrics.createIndex(
  { domain_id: 1, snapshot_date: -1 },
  { name: "ix_metrics_domain_id_date" }
);
db.domain_daily_metrics.createIndex(
  { snapshot_date: -1 },
  { name: "ix_metrics_date" }
);

db.collection_jobs.createIndex(
  { status: 1, queued_at: 1 },
  { name: "ix_jobs_status_queue" }
);
db.collection_jobs.createIndex(
  { domain_id: 1, snapshot_date: -1 },
  { name: "ix_jobs_domain_date" }
);
db.collection_jobs.createIndex(
  { domain_id: 1, queued_at: -1 },
  { name: "ix_jobs_domain_latest" }
);
db.collection_jobs.createIndex(
  { snapshot_date: -1 },
  { name: "ix_jobs_date" }
);
// dedupe_key 只在 queued/running 状态存在，防止同一域名同一天重复排队。
db.collection_jobs.createIndex(
  { dedupe_key: 1 },
  { name: "uq_jobs_open", unique: true, sparse: true }
);

db.domain_certificates.createIndex(
  { domain_id: 1 },
  { name: "uq_certificates_domain_id", unique: true }
);
db.domain_certificates.createIndex(
  { expires_at: 1 },
  { name: "ix_certificates_expires" }
);
db.domain_certificates.createIndex(
  { checked_at: -1 },
  { name: "ix_certificates_checked" }
);
db.domain_certificate_history.createIndex(
  { domain_id: 1, check_date: -1 },
  { name: "ix_certificate_history_domain_date" }
);
db.domain_certificate_history.createIndex(
  { check_date: -1 },
  { name: "ix_certificate_history_date" }
);
db.domain_certificate_history.createIndex(
  { domain_id: 1, checked_at: -1 },
  { name: "ix_certificate_history_domain_checked" }
);

db.users.createIndex(
  { username: 1 },
  { name: "uq_users_username", unique: true }
);
db.auth_sessions.createIndex(
  { token_hash: 1 },
  { name: "uq_auth_sessions_token", unique: true }
);
db.auth_sessions.createIndex(
  { expires_at: 1 },
  { name: "ttl_auth_sessions_expiry", expireAfterSeconds: 0 }
);
db.auth_sessions.createIndex(
  { user_id: 1 },
  { name: "ix_auth_sessions_user" }
);

print("seo_monitor database, validators, and indexes are ready; the Go service creates the default admin on startup.");
