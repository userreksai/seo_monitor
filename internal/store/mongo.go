package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"golang.org/x/crypto/bcrypt"

	"seo-monitor/internal/model"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalidSearch = errors.New("invalid search")
)

type latestSearchField struct {
	Path    string
	Numeric bool
}

var latestSearchFields = map[string]latestSearchField{
	"domain":              {Path: "domain"},
	"display_name":        {Path: "display_name"},
	"site_category":       {Path: "metric.site_category"},
	"registrant_name":     {Path: "metric.registrant_name"},
	"registrant_email":    {Path: "metric.registrant_email"},
	"baidu_pc_weight":     {Path: "metric.baidu_pc_weight", Numeric: true},
	"baidu_mobile_weight": {Path: "metric.baidu_mobile_weight", Numeric: true},
	"sogou_weight":        {Path: "metric.sogou_weight", Numeric: true},
	"bing_weight":         {Path: "metric.bing_weight", Numeric: true},
	"so_360_weight":       {Path: "metric.so_360_weight", Numeric: true},
	"shenma_weight":       {Path: "metric.shenma_weight", Numeric: true},
	"pr_weight":           {Path: "metric.pr_weight", Numeric: true},
	"apppc_pc_rank":       {Path: "metric.apppc_pc_rank", Numeric: true},
	"backlink_count":      {Path: "metric.backlink_count", Numeric: true},
	"traffic_min":         {Path: "metric.traffic_min", Numeric: true},
	"traffic_max":         {Path: "metric.traffic_max", Numeric: true},
	"domain_age_days":     {Path: "metric.domain_age_days", Numeric: true},
}

type Store struct {
	client             *mongo.Client
	db                 *mongo.Database
	domains            *mongo.Collection
	metrics            *mongo.Collection
	jobs               *mongo.Collection
	certificates       *mongo.Collection
	certificateHistory *mongo.Collection
	users              *mongo.Collection
	sessions           *mongo.Collection
}

// CleanupResult reports how many records were removed from each retained collection.
type CleanupResult struct {
	MetricsDeleted int64
	JobsDeleted    int64
}

func New(ctx context.Context, uri, database string) (*Store, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetAppName("seo-monitor"))
	if err != nil {
		return nil, fmt.Errorf("connect MongoDB: %w", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping MongoDB: %w", err)
	}
	db := client.Database(database)
	return &Store{
		client:             client,
		db:                 db,
		domains:            db.Collection("domains"),
		metrics:            db.Collection("domain_daily_metrics"),
		jobs:               db.Collection("collection_jobs"),
		certificates:       db.Collection("domain_certificates"),
		certificateHistory: db.Collection("domain_certificate_history"),
		users:              db.Collection("users"),
		sessions:           db.Collection("auth_sessions"),
	}, nil
}

func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

func (s *Store) Health(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}

// EnsureIndexes is safe to call on every startup. Collection validators live in
// scripts/mongo-init.js so operators can review and apply them explicitly.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.domains.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain", Value: 1}}, Options: options.Index().SetName("uq_domains_domain").SetUnique(true)},
		{Keys: bson.D{{Key: "active", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("ix_domains_active_created")},
		{Keys: bson.D{{Key: "certificate_active", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("ix_domains_certificate_active_created")},
	})
	if err != nil {
		return fmt.Errorf("create domain indexes: %w", err)
	}

	_, err = s.metrics.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain", Value: 1}, {Key: "snapshot_date", Value: 1}}, Options: options.Index().SetName("uq_metrics_domain_date").SetUnique(true)},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_metrics_domain_id_date")},
		{Keys: bson.D{{Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_metrics_date")},
	})
	if err != nil {
		return fmt.Errorf("create metric indexes: %w", err)
	}

	_, err = s.jobs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "queued_at", Value: 1}}, Options: options.Index().SetName("ix_jobs_status_queue")},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_jobs_domain_date")},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "queued_at", Value: -1}}, Options: options.Index().SetName("ix_jobs_domain_latest")},
		{Keys: bson.D{{Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_jobs_date")},
		{Keys: bson.D{{Key: "dedupe_key", Value: 1}}, Options: options.Index().SetName("uq_jobs_open").SetUnique(true).SetSparse(true)},
	})
	if err != nil {
		return fmt.Errorf("create job indexes: %w", err)
	}

	_, err = s.certificates.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain_id", Value: 1}}, Options: options.Index().SetName("uq_certificates_domain_id").SetUnique(true)},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("ix_certificates_expires")},
		{Keys: bson.D{{Key: "checked_at", Value: -1}}, Options: options.Index().SetName("ix_certificates_checked")},
	})
	if err != nil {
		return fmt.Errorf("create certificate indexes: %w", err)
	}
	_, err = s.certificateHistory.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "check_date", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_domain_date")},
		{Keys: bson.D{{Key: "check_date", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_date")},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "checked_at", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_domain_checked")},
	})
	if err != nil {
		return fmt.Errorf("create certificate history indexes: %w", err)
	}
	return nil
}

// InitializeAuth is safe to run at every startup. It creates the authentication
// indexes and inserts the default administrator only when that username does not
// already exist; subsequent restarts never reset a changed password.
func (s *Store) InitializeAuth(ctx context.Context, username, password string) error {
	username = normalizeUsername(username)
	if username == "" || password == "" {
		return errors.New("default administrator username and password are required")
	}
	if _, err := s.users.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "username", Value: 1}},
		Options: options.Index().SetName("uq_users_username").SetUnique(true),
	}); err != nil {
		return fmt.Errorf("create user index: %w", err)
	}
	if _, err := s.sessions.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "token_hash", Value: 1}}, Options: options.Index().SetName("uq_auth_sessions_token").SetUnique(true)},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("ttl_auth_sessions_expiry").SetExpireAfterSeconds(0)},
		{Keys: bson.D{{Key: "user_id", Value: 1}}, Options: options.Index().SetName("ix_auth_sessions_user")},
	}); err != nil {
		return fmt.Errorf("create auth session indexes: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash default administrator password: %w", err)
	}
	now := time.Now().UTC()
	result, err := s.users.UpdateOne(ctx, bson.M{"username": username}, bson.M{"$setOnInsert": model.User{
		ID: primitive.NewObjectID(), Username: username, PasswordHash: string(hash), Role: "admin",
		Active: true, CreatedAt: now, UpdatedAt: now,
	}}, options.Update().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("initialize default administrator: %w", err)
	}
	_ = result
	return nil
}

func (s *Store) AuthenticateUser(ctx context.Context, username, password string) (model.User, error) {
	username = normalizeUsername(username)
	var user model.User
	err := s.users.FindOne(ctx, bson.M{"username": username, "active": true}).Decode(&user)
	if errors.Is(err, mongo.ErrNoDocuments) {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(password))
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		return model.User{}, ErrNotFound
	}
	now := time.Now().UTC()
	if _, err := s.users.UpdateOne(ctx, bson.M{"_id": user.ID}, bson.M{"$set": bson.M{
		"last_login_at": now, "updated_at": now,
	}}); err != nil {
		return model.User{}, err
	}
	user.LastLoginAt = &now
	user.UpdatedAt = now
	return user, nil
}

func (s *Store) ChangePassword(ctx context.Context, userID primitive.ObjectID, currentPassword, newPassword string) error {
	if err := validateNewPassword(newPassword); err != nil {
		return err
	}
	var user model.User
	if err := s.users.FindOne(ctx, bson.M{"_id": userID, "active": true}).Decode(&user); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return ErrNotFound
		}
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(currentPassword)) != nil {
		return ErrNotFound
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := s.users.UpdateOne(ctx, bson.M{"_id": userID, "password_hash": user.PasswordHash}, bson.M{"$set": bson.M{
		"password_hash": string(hash), "password_changed_at": now, "updated_at": now,
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return ErrNotFound
	}
	// password_changed_at is the authoritative revocation marker; deletion is
	// best-effort cleanup so a cleanup failure cannot leave a usable old session.
	_, _ = s.sessions.DeleteMany(ctx, bson.M{"user_id": userID})
	return nil
}

func (s *Store) CreateSession(ctx context.Context, userID primitive.ObjectID, ttl time.Duration) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token := hex.EncodeToString(raw)
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	_, err := s.sessions.InsertOne(ctx, model.AuthSession{
		ID: primitive.NewObjectID(), UserID: userID, TokenHash: tokenDigest(token),
		CreatedAt: now, ExpiresAt: expiresAt,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

func (s *Store) AuthenticateSession(ctx context.Context, token string) (model.User, error) {
	if len(token) != 64 {
		return model.User{}, ErrNotFound
	}
	var session model.AuthSession
	err := s.sessions.FindOne(ctx, bson.M{
		"token_hash": tokenDigest(token), "expires_at": bson.M{"$gt": time.Now().UTC()},
	}).Decode(&session)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	var user model.User
	err = s.users.FindOne(ctx, bson.M{"_id": session.UserID, "active": true}).Decode(&user)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.User{}, ErrNotFound
	}
	if err != nil {
		return model.User{}, err
	}
	if user.PasswordChangedAt != nil && !session.CreatedAt.After(*user.PasswordChangedAt) {
		return model.User{}, ErrNotFound
	}
	return user, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	_, err := s.sessions.DeleteOne(ctx, bson.M{"token_hash": tokenDigest(token)})
	return err
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

var dummyPasswordHash = func() []byte {
	hash, _ := bcrypt.GenerateFromPassword([]byte("invalid-password-placeholder"), bcrypt.DefaultCost)
	return hash
}()

func (s *Store) CreateDomain(ctx context.Context, domain string, displayName *string) (model.Domain, error) {
	now := time.Now().UTC()
	item := model.Domain{
		ID:          primitive.NewObjectID(),
		Domain:      domain,
		DisplayName: displayName,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.domains.InsertOne(ctx, item); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return model.Domain{}, ErrConflict
		}
		return model.Domain{}, err
	}
	return item, nil
}

// ActivateMetricDomain makes a file-configured domain available to metric
// collection. It also reactivates a certificate-only record when the same
// hostname is later added to the metric domains file.
func (s *Store) ActivateMetricDomain(ctx context.Context, domain string) (bool, error) {
	now := time.Now().UTC()
	result, err := s.domains.UpdateOne(ctx, bson.M{"domain": domain}, bson.M{
		"$set":   bson.M{"active": true, "updated_at": now},
		"$unset": bson.M{"archived_at": ""},
		"$setOnInsert": bson.M{
			"_id": primitive.NewObjectID(), "domain": domain, "created_at": now,
		},
	}, options.Update().SetUpsert(true))
	if err != nil {
		return false, err
	}
	return result.UpsertedCount > 0, nil
}

// SyncCertificateDomains replaces certificate membership with the supplied
// file contents without changing metric collection membership.
func (s *Store) SyncCertificateDomains(ctx context.Context, domains []string) error {
	now := time.Now().UTC()
	if _, err := s.domains.UpdateMany(ctx, bson.M{"certificate_active": true}, bson.M{
		"$set": bson.M{"certificate_active": false, "updated_at": now},
	}); err != nil {
		return err
	}
	for _, domain := range domains {
		if _, err := s.domains.UpdateOne(ctx, bson.M{"domain": domain}, bson.M{
			"$set": bson.M{"certificate_active": true, "updated_at": now},
			"$setOnInsert": bson.M{
				"_id": primitive.NewObjectID(), "domain": domain, "active": false, "created_at": now,
			},
		}, options.Update().SetUpsert(true)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ListCertificateDomains(ctx context.Context) ([]model.Domain, error) {
	cursor, err := s.domains.Find(ctx, bson.M{"certificate_active": true},
		options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Domain
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) ListDomains(ctx context.Context, includeArchived bool) ([]model.Domain, error) {
	filter := bson.M{}
	if !includeArchived {
		filter["active"] = true
	}
	cursor, err := s.domains.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Domain
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) GetDomain(ctx context.Context, id primitive.ObjectID) (model.Domain, error) {
	var item model.Domain
	err := s.domains.FindOne(ctx, bson.M{"_id": id}).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.Domain{}, ErrNotFound
	}
	return item, err
}

func (s *Store) UpdateDomain(ctx context.Context, id primitive.ObjectID, patch model.DomainPatch) (model.Domain, error) {
	set := bson.M{"updated_at": time.Now().UTC()}
	unset := bson.M{}
	if patch.HasDisplayName {
		if patch.DisplayName == "" {
			unset["display_name"] = ""
		} else {
			set["display_name"] = patch.DisplayName
		}
	}
	if patch.HasActive {
		set["active"] = patch.Active
		if patch.Active {
			unset["archived_at"] = ""
		} else {
			set["archived_at"] = time.Now().UTC()
		}
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	var item model.Domain
	err := s.domains.FindOneAndUpdate(ctx, bson.M{"_id": id}, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.Domain{}, ErrNotFound
	}
	return item, err
}

func (s *Store) ArchiveDomain(ctx context.Context, id primitive.ObjectID) error {
	now := time.Now().UTC()
	result, err := s.domains.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"active": false, "archived_at": now, "updated_at": now,
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return ErrNotFound
	}
	_, _ = s.jobs.UpdateMany(ctx, bson.M{"domain_id": id, "status": "queued"}, bson.M{
		"$set":   bson.M{"status": "canceled", "finished_at": now},
		"$unset": bson.M{"dedupe_key": ""},
	})
	return nil
}

func (s *Store) QueueAll(ctx context.Context, snapshotDate time.Time, requestedBy string, force bool) (int, error) {
	domains, err := s.ListDomains(ctx, false)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, domain := range domains {
		_, added, queueErr := s.QueueDomain(ctx, domain.ID, snapshotDate, requestedBy, force)
		if queueErr != nil {
			return queued, queueErr
		}
		if added {
			queued++
		}
	}
	return queued, nil
}

func (s *Store) QueueDomain(ctx context.Context, id primitive.ObjectID, snapshotDate time.Time, requestedBy string, force bool) (model.CollectionJob, bool, error) {
	domain, err := s.GetDomain(ctx, id)
	if err != nil {
		return model.CollectionJob{}, false, err
	}
	if !domain.Active {
		return model.CollectionJob{}, false, fmt.Errorf("域名已归档")
	}
	if !force {
		count, countErr := s.jobs.CountDocuments(ctx, bson.M{
			"domain_id": id, "snapshot_date": snapshotDate, "status": "succeeded",
		}, options.Count().SetLimit(1))
		if countErr != nil {
			return model.CollectionJob{}, false, countErr
		}
		if count > 0 {
			return model.CollectionJob{}, false, nil
		}
	}

	now := time.Now().UTC()
	key := id.Hex() + ":" + snapshotDate.Format("2006-01-02")
	job := model.CollectionJob{
		ID:           primitive.NewObjectID(),
		DomainID:     id,
		Domain:       domain.Domain,
		SnapshotDate: snapshotDate,
		Status:       "queued",
		RequestedBy:  requestedBy,
		QueuedAt:     now,
		DedupeKey:    &key,
	}
	if _, err := s.jobs.InsertOne(ctx, job); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return model.CollectionJob{}, false, nil
		}
		return model.CollectionJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) ClaimNextJob(ctx context.Context) (model.CollectionJob, error) {
	now := time.Now().UTC()
	update := bson.M{
		"$set": bson.M{"status": "running", "started_at": now},
		"$inc": bson.M{"attempt_count": 1},
	}
	var job model.CollectionJob
	err := s.jobs.FindOneAndUpdate(ctx, bson.M{"status": "queued"}, update,
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "queued_at", Value: 1}}).SetReturnDocument(options.After),
	).Decode(&job)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.CollectionJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) SaveJobResult(ctx context.Context, job model.CollectionJob, metric model.Metric) error {
	metric.ID = primitive.NilObjectID
	metric.DomainID = job.DomainID
	metric.Domain = job.Domain
	metric.SnapshotDate = job.SnapshotDate
	if metric.CollectedAt.IsZero() {
		metric.CollectedAt = time.Now().UTC()
	}
	_, err := s.metrics.ReplaceOne(ctx,
		bson.M{"domain": job.Domain, "snapshot_date": job.SnapshotDate}, metric,
		options.Replace().SetUpsert(true))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.jobs.UpdateOne(ctx, bson.M{"_id": job.ID}, bson.M{
		"$set":   bson.M{"status": "succeeded", "finished_at": now},
		"$unset": bson.M{"dedupe_key": "", "error_message": ""},
	})
	return err
}

func (s *Store) MarkJobFailed(ctx context.Context, id primitive.ObjectID, cause error) error {
	message := cause.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	_, err := s.jobs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set":   bson.M{"status": "failed", "finished_at": time.Now().UTC(), "error_message": message},
		"$unset": bson.M{"dedupe_key": ""},
	})
	return err
}

func (s *Store) RecoverStaleJobs(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	result, err := s.jobs.UpdateMany(ctx, bson.M{"status": "running", "started_at": bson.M{"$lt": cutoff}}, bson.M{
		"$set":   bson.M{"status": "queued", "queued_at": time.Now().UTC()},
		"$unset": bson.M{"started_at": ""},
	})
	if err != nil {
		return 0, err
	}
	return result.ModifiedCount, nil
}

// CleanupBefore removes historical metrics and collection/polling records whose
// snapshot date is strictly older than cutoff. The cutoff date itself is kept.
func (s *Store) CleanupBefore(ctx context.Context, cutoff time.Time) (CleanupResult, error) {
	var cleaned CleanupResult
	if cutoff.IsZero() {
		return cleaned, fmt.Errorf("cleanup cutoff must not be zero")
	}
	filter := snapshotDateBeforeFilter(cutoff)

	jobsResult, err := s.jobs.DeleteMany(ctx, filter)
	if err != nil {
		return cleaned, fmt.Errorf("delete expired collection jobs: %w", err)
	}
	cleaned.JobsDeleted = jobsResult.DeletedCount

	metricsResult, err := s.metrics.DeleteMany(ctx, filter)
	if err != nil {
		return cleaned, fmt.Errorf("delete expired daily metrics: %w", err)
	}
	cleaned.MetricsDeleted = metricsResult.DeletedCount
	return cleaned, nil
}

func snapshotDateBeforeFilter(cutoff time.Time) bson.M {
	return bson.M{"snapshot_date": bson.M{"$lt": cutoff}}
}

func (s *Store) Metrics(ctx context.Context, id primitive.ObjectID, from, to time.Time) ([]model.Metric, error) {
	filter := bson.M{"domain_id": id, "snapshot_date": bson.M{"$gte": from, "$lte": to}}
	cursor, err := s.metrics.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "snapshot_date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Metric
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) LatestMetric(ctx context.Context, id primitive.ObjectID) (*model.Metric, error) {
	var metric model.Metric
	err := s.metrics.FindOne(ctx, bson.M{"domain_id": id}, options.FindOne().SetSort(bson.D{{Key: "snapshot_date", Value: -1}})).Decode(&metric)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return &metric, err
}

// SaveCertificate appends every polling attempt to history, then replaces the
// latest state used by the certificate dashboard.
func (s *Store) SaveCertificate(ctx context.Context, item model.Certificate) error {
	if item.CheckedAt.IsZero() {
		return fmt.Errorf("certificate checked_at must not be zero")
	}
	if item.CheckDate.IsZero() {
		year, month, day := item.CheckedAt.UTC().Date()
		item.CheckDate = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	}
	item.ID = primitive.NilObjectID
	_, err := s.certificateHistory.InsertOne(ctx, item)
	if err != nil {
		return fmt.Errorf("save certificate polling history: %w", err)
	}
	_, err = s.certificates.ReplaceOne(ctx, bson.M{"domain_id": item.DomainID}, item,
		options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("save latest certificate: %w", err)
	}
	return nil
}

// CertificateHistory returns retained polling results for a domain, newest
// first.
func (s *Store) CertificateHistory(ctx context.Context, id primitive.ObjectID, limit int64) ([]model.Certificate, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	cursor, err := s.certificateHistory.Find(ctx, bson.M{"domain_id": id},
		options.Find().SetSort(bson.D{{Key: "check_date", Value: -1}, {Key: "checked_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Certificate
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	if items == nil {
		items = []model.Certificate{}
	}
	return items, nil
}

// CleanupCertificateHistory keeps all polling results inside the normal
// retention window. Older failures are removed, while successful results keep
// an additional window ending at the most recent success for each domain. This
// preserves a week of known-good data during a prolonged outage without
// retaining every historical failure.
func (s *Store) CleanupCertificateHistory(ctx context.Context, cutoff time.Time, retentionDays int) (int64, error) {
	if cutoff.IsZero() {
		return 0, fmt.Errorf("certificate cleanup cutoff must not be zero")
	}
	if retentionDays < 1 {
		return 0, fmt.Errorf("certificate retention days must be positive")
	}

	oldFailures, err := s.certificateHistory.DeleteMany(ctx, bson.M{
		"check_date":    bson.M{"$type": "date", "$lt": cutoff},
		"error_message": bson.M{"$type": "string", "$ne": ""},
	})
	if err != nil {
		return 0, fmt.Errorf("delete expired certificate failures: %w", err)
	}
	deleted := oldFailures.DeletedCount

	cursor, err := s.certificateHistory.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: certificateHistorySuccessMatch()}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":            "$domain_id",
			"latest_success": bson.M{"$max": "$check_date"},
		}}},
	})
	if err != nil {
		return deleted, fmt.Errorf("find latest certificate successes: %w", err)
	}
	defer cursor.Close(ctx)
	var latestSuccesses []struct {
		DomainID      primitive.ObjectID `bson:"_id"`
		LatestSuccess time.Time          `bson:"latest_success"`
	}
	if err := cursor.All(ctx, &latestSuccesses); err != nil {
		return deleted, fmt.Errorf("decode latest certificate successes: %w", err)
	}

	models := make([]mongo.WriteModel, 0, len(latestSuccesses))
	for _, success := range latestSuccesses {
		successCutoff := certificateSuccessRetentionCutoff(cutoff, success.LatestSuccess, retentionDays)
		filter := certificateHistorySuccessMatch()
		filter["domain_id"] = success.DomainID
		filter["check_date"] = bson.M{"$type": "date", "$lt": successCutoff}
		models = append(models, mongo.NewDeleteManyModel().SetFilter(filter))
	}
	if len(models) == 0 {
		return deleted, nil
	}
	result, err := s.certificateHistory.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return deleted, fmt.Errorf("delete expired certificate successes: %w", err)
	}
	return deleted + result.DeletedCount, nil
}

func certificateHistorySuccessMatch() bson.M {
	return bson.M{
		"check_date": bson.M{"$type": "date"},
		"$or": bson.A{
			bson.M{"error_message": bson.M{"$exists": false}},
			bson.M{"error_message": nil},
			bson.M{"error_message": ""},
		},
	}
}

func certificateSuccessRetentionCutoff(cutoff, latestSuccess time.Time, retentionDays int) time.Time {
	if latestSuccess.IsZero() {
		return cutoff
	}
	successCutoff := latestSuccess.AddDate(0, 0, -retentionDays)
	if successCutoff.Before(cutoff) {
		return successCutoff
	}
	return cutoff
}

// ListCertificates returns certificate-configured domains joined with their latest certificate
// check. Summary counts cover the complete query result before the optional
// status filter is applied, so dashboard cards remain independent of paging.
func (s *Store) ListCertificates(ctx context.Context, query, status string, page, limit int64) ([]model.LatestCertificate, int64, model.CertificateSummary, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	now := time.Now().UTC()
	statusMatch, err := certificateStatusMatch(status, now)
	if err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}

	match := bson.M{"certificate_active": true}
	if query = strings.TrimSpace(query); query != "" {
		pattern := primitive.Regex{Pattern: regexp.QuoteMeta(query), Options: "i"}
		match["$or"] = bson.A{
			bson.M{"domain": pattern},
			bson.M{"display_name": pattern},
		}
	}

	itemsPipeline := mongo.Pipeline{}
	totalPipeline := mongo.Pipeline{}
	if len(statusMatch) > 0 {
		itemsPipeline = append(itemsPipeline, bson.D{{Key: "$match", Value: statusMatch}})
		totalPipeline = append(totalPipeline, bson.D{{Key: "$match", Value: statusMatch}})
	}
	itemsPipeline = append(itemsPipeline,
		bson.D{{Key: "$sort", Value: bson.D{{Key: "domain", Value: 1}}}},
		bson.D{{Key: "$skip", Value: (page - 1) * limit}},
		bson.D{{Key: "$limit", Value: limit}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "domain_record", Value: bson.D{
				{Key: "_id", Value: "$_id"},
				{Key: "domain", Value: "$domain"},
				{Key: "display_name", Value: "$display_name"},
				{Key: "active", Value: "$active"},
				{Key: "certificate_active", Value: "$certificate_active"},
				{Key: "created_at", Value: "$created_at"},
				{Key: "updated_at", Value: "$updated_at"},
				{Key: "archived_at", Value: "$archived_at"},
			}},
			{Key: "certificate", Value: "$certificate"},
		}}},
	)
	totalPipeline = append(totalPipeline, bson.D{{Key: "$count", Value: "value"}})

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from":         "domain_certificates",
			"localField":   "_id",
			"foreignField": "domain_id",
			"as":           "certificate_docs",
		}}},
		bson.D{{Key: "$set", Value: bson.M{
			"certificate": bson.M{"$ifNull": bson.A{
				bson.M{"$arrayElemAt": bson.A{"$certificate_docs", 0}}, nil,
			}},
		}}},
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "items", Value: itemsPipeline},
			{Key: "total", Value: totalPipeline},
			{Key: "summary", Value: mongo.Pipeline{
				bson.D{{Key: "$group", Value: bson.M{
					"_id":   nil,
					"total": bson.M{"$sum": 1},
					"checked": bson.M{"$sum": bson.M{"$cond": bson.A{
						bson.M{"$ne": bson.A{"$certificate", nil}}, 1, 0,
					}}},
					"expiring_soon": bson.M{"$sum": bson.M{"$cond": bson.A{
						certificateExpiryCondition(now, now.Add(30*24*time.Hour)), 1, 0,
					}}},
					"expired": bson.M{"$sum": bson.M{"$cond": bson.A{
						certificateExpiredCondition(now), 1, 0,
					}}},
					"failed": bson.M{"$sum": bson.M{"$cond": bson.A{
						bson.M{"$ne": bson.A{
							bson.M{"$ifNull": bson.A{"$certificate.error_message", ""}}, "",
						}}, 1, 0,
					}}},
				}}},
			}},
		}}},
	}

	cursor, err := s.domains.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}
	defer cursor.Close(ctx)
	var result []struct {
		Items []model.LatestCertificate `bson:"items"`
		Total []struct {
			Value int64 `bson:"value"`
		} `bson:"total"`
		Summary []model.CertificateSummary `bson:"summary"`
	}
	if err := cursor.All(ctx, &result); err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}
	if len(result) == 0 {
		return []model.LatestCertificate{}, 0, model.CertificateSummary{}, nil
	}
	total := int64(0)
	if len(result[0].Total) > 0 {
		total = result[0].Total[0].Value
	}
	summary := model.CertificateSummary{}
	if len(result[0].Summary) > 0 {
		summary = result[0].Summary[0]
	}
	return result[0].Items, total, summary, nil
}

func certificateStatusMatch(status string, now time.Time) (bson.D, error) {
	switch strings.TrimSpace(status) {
	case "":
		return nil, nil
	case "checked":
		return bson.D{{Key: "certificate", Value: bson.M{"$ne": nil}}}, nil
	case "expiring":
		return bson.D{{Key: "certificate.expires_at", Value: bson.M{
			"$type": "date",
			"$gte":  now,
			"$lte":  now.Add(30 * 24 * time.Hour),
		}}}, nil
	case "expired":
		return bson.D{{Key: "certificate.expires_at", Value: bson.M{"$type": "date", "$lt": now}}}, nil
	case "failed":
		return bson.D{{Key: "certificate.error_message", Value: bson.M{
			"$exists": true,
			"$type":   "string",
			"$ne":     "",
		}}}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported certificate status %q", ErrInvalidSearch, status)
	}
}

func certificateExpiryCondition(now, cutoff time.Time) bson.M {
	return bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$certificate.expires_at"}, "date"}},
		bson.M{"$gte": bson.A{"$certificate.expires_at", now}},
		bson.M{"$lte": bson.A{"$certificate.expires_at", cutoff}},
	}}
}

func certificateExpiredCondition(now time.Time) bson.M {
	return bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$certificate.expires_at"}, "date"}},
		bson.M{"$lt": bson.A{"$certificate.expires_at", now}},
	}}
}

func (s *Store) ListJobs(ctx context.Context, status string, limit int64) ([]model.CollectionJob, error) {
	filter := bson.M{}
	if status != "" {
		filter["status"] = status
	}
	cursor, err := s.jobs.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "queued_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.CollectionJob
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// CollectionProgress returns the latest job state for every active domain on
// one collection date. Starting from domains keeps never-queued domains in the
// pending count and avoids double-counting retries.
func (s *Store) CollectionProgress(ctx context.Context, snapshotDate time.Time) (model.CollectionProgress, error) {
	progress := model.CollectionProgress{SnapshotDate: snapshotDate}
	statusCount := func(status string) bson.M {
		return bson.M{"$sum": bson.M{"$cond": bson.A{
			bson.M{"$eq": bson.A{"$collection_status", status}}, 1, 0,
		}}}
	}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"active": true}}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from": "collection_jobs",
			"let":  bson.M{"domainID": "$_id"},
			"pipeline": mongo.Pipeline{
				bson.D{{Key: "$match", Value: bson.M{"$expr": bson.M{"$and": bson.A{
					bson.M{"$eq": bson.A{"$domain_id", "$$domainID"}},
					bson.M{"$eq": bson.A{"$snapshot_date", snapshotDate}},
				}}}}},
				bson.D{{Key: "$sort", Value: bson.D{{Key: "queued_at", Value: -1}}}},
				bson.D{{Key: "$limit", Value: 1}},
			},
			"as": "collection_jobs",
		}}},
		bson.D{{Key: "$set", Value: bson.M{
			"collection_status": bson.M{"$ifNull": bson.A{
				bson.M{"$arrayElemAt": bson.A{"$collection_jobs.status", 0}},
				"pending",
			}},
		}}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":       nil,
			"total":     bson.M{"$sum": 1},
			"pending":   statusCount("pending"),
			"queued":    statusCount("queued"),
			"running":   statusCount("running"),
			"succeeded": statusCount("succeeded"),
			"failed":    statusCount("failed"),
			"canceled":  statusCount("canceled"),
		}}},
	}
	cursor, err := s.domains.Aggregate(ctx, pipeline)
	if err != nil {
		return progress, err
	}
	defer cursor.Close(ctx)
	var results []struct {
		Total     int64 `bson:"total"`
		Pending   int64 `bson:"pending"`
		Queued    int64 `bson:"queued"`
		Running   int64 `bson:"running"`
		Succeeded int64 `bson:"succeeded"`
		Failed    int64 `bson:"failed"`
		Canceled  int64 `bson:"canceled"`
	}
	if err := cursor.All(ctx, &results); err != nil {
		return progress, err
	}
	if len(results) == 0 {
		return progress, nil
	}
	result := results[0]
	progress.Total = result.Total
	progress.Pending = result.Pending
	progress.Queued = result.Queued
	progress.Running = result.Running
	progress.Succeeded = result.Succeeded
	progress.Failed = result.Failed
	progress.Canceled = result.Canceled
	progress.Completed = result.Succeeded + result.Failed + result.Canceled
	progress.InProgress = result.Queued > 0 || result.Running > 0
	return progress, nil
}

// SearchLatest returns active domains with their newest metric snapshot. Only
// fields in latestSearchFields are accepted, preventing arbitrary MongoDB paths
// from being supplied by API callers.
func (s *Store) SearchLatest(ctx context.Context, field, query, status string, page, limit int64) ([]model.LatestMetric, int64, error) {
	match, err := latestSearchMatch(field, query)
	if err != nil {
		return nil, 0, err
	}
	statusMatch, err := latestCollectionStatusMatch(status)
	if err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}

	lookupStage := bson.D{{Key: "$lookup", Value: bson.M{
		"from": "domain_daily_metrics",
		"let":  bson.M{"domainID": "$_id"},
		"pipeline": mongo.Pipeline{
			bson.D{{Key: "$match", Value: bson.M{
				"$expr": bson.M{"$eq": bson.A{"$domain_id", "$$domainID"}},
			}}},
			bson.D{{Key: "$sort", Value: bson.M{"snapshot_date": -1}}},
			bson.D{{Key: "$limit", Value: 1}},
		},
		"as": "metric_docs",
	}}}
	setMetricStage := bson.D{{Key: "$set", Value: bson.M{
		"metric": bson.M{"$arrayElemAt": bson.A{"$metric_docs", 0}},
	}}}
	collectionLookupStage := bson.D{{Key: "$lookup", Value: bson.M{
		"from": "collection_jobs",
		"let":  bson.M{"domainID": "$_id"},
		"pipeline": mongo.Pipeline{
			bson.D{{Key: "$match", Value: bson.M{
				"$expr": bson.M{"$eq": bson.A{"$domain_id", "$$domainID"}},
			}}},
			bson.D{{Key: "$sort", Value: bson.D{{Key: "queued_at", Value: -1}}}},
			bson.D{{Key: "$limit", Value: 1}},
		},
		"as": "collection_docs",
	}}}
	setCollectionStage := bson.D{{Key: "$set", Value: bson.M{
		"collection": bson.M{"$arrayElemAt": bson.A{"$collection_docs", 0}},
	}}}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"active": true}}},
		lookupStage,
		setMetricStage,
		collectionLookupStage,
		setCollectionStage,
	}
	if len(match) > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: match}})
	}
	if len(statusMatch) > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: statusMatch}})
	}
	pipeline = append(pipeline,
		bson.D{{Key: "$sort", Value: bson.D{{Key: "domain", Value: 1}}}},
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "items", Value: mongo.Pipeline{
				bson.D{{Key: "$skip", Value: (page - 1) * limit}},
				bson.D{{Key: "$limit", Value: limit}},
				bson.D{{Key: "$project", Value: bson.D{
					{Key: "_id", Value: 0},
					{Key: "domain_record", Value: bson.D{
						{Key: "_id", Value: "$_id"},
						{Key: "domain", Value: "$domain"},
						{Key: "display_name", Value: "$display_name"},
						{Key: "active", Value: "$active"},
						{Key: "created_at", Value: "$created_at"},
						{Key: "updated_at", Value: "$updated_at"},
						{Key: "archived_at", Value: "$archived_at"},
					}},
					{Key: "metric", Value: "$metric"},
					{Key: "collection", Value: "$collection"},
				}}},
			}},
			{Key: "total", Value: mongo.Pipeline{
				bson.D{{Key: "$count", Value: "value"}},
			}},
		}}},
	)

	cursor, err := s.domains.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)
	var result []struct {
		Items []model.LatestMetric `bson:"items"`
		Total []struct {
			Value int64 `bson:"value"`
		} `bson:"total"`
	}
	if err := cursor.All(ctx, &result); err != nil {
		return nil, 0, err
	}
	if len(result) == 0 {
		return []model.LatestMetric{}, 0, nil
	}
	total := int64(0)
	if len(result[0].Total) > 0 {
		total = result[0].Total[0].Value
	}
	return result[0].Items, total, nil
}

func latestCollectionStatusMatch(status string) (bson.D, error) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "":
		return nil, nil
	case "failed":
		return bson.D{{Key: "collection.status", Value: "failed"}}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported collection status %q", ErrInvalidSearch, status)
	}
}

func latestSearchMatch(field, query string) (bson.D, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		field = "domain"
	}
	spec, ok := latestSearchFields[field]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported field %q", ErrInvalidSearch, field)
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if spec.Numeric {
		value, err := strconv.ParseInt(query, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: field %s requires an integer", ErrInvalidSearch, field)
		}
		return bson.D{{Key: spec.Path, Value: value}}, nil
	}
	return bson.D{{Key: spec.Path, Value: primitive.Regex{Pattern: regexp.QuoteMeta(query), Options: "i"}}}, nil
}
