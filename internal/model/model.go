package model

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Domain is a monitored hostname. Deleting a domain through the API archives it
// so that historical metrics remain queryable.
type Domain struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	Domain      string             `bson:"domain" json:"domain"`
	DisplayName *string            `bson:"display_name,omitempty" json:"display_name,omitempty"`
	Active      bool               `bson:"active" json:"active"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at" json:"updated_at"`
	ArchivedAt  *time.Time         `bson:"archived_at,omitempty" json:"archived_at,omitempty"`
}

type DomainPatch struct {
	DisplayName    string
	HasDisplayName bool
	Active         bool
	HasActive      bool
}

// Metric is the once-per-day snapshot. Pointers preserve the distinction
// between a real zero and a value that the source did not return.
type Metric struct {
	ID              primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	DomainID        primitive.ObjectID `bson:"domain_id" json:"domain_id"`
	Domain          string             `bson:"domain" json:"domain"`
	SnapshotDate    time.Time          `bson:"snapshot_date" json:"snapshot_date"`
	CollectedAt     time.Time          `bson:"collected_at" json:"collected_at"`
	TrafficText     *string            `bson:"traffic_text,omitempty" json:"traffic_text,omitempty"`
	TrafficMin      *int64             `bson:"traffic_min,omitempty" json:"traffic_min,omitempty"`
	TrafficMax      *int64             `bson:"traffic_max,omitempty" json:"traffic_max,omitempty"`
	BaiduPCWeight   *int16             `bson:"baidu_pc_weight,omitempty" json:"baidu_pc_weight,omitempty"`
	BaiduMobile     *int16             `bson:"baidu_mobile_weight,omitempty" json:"baidu_mobile_weight,omitempty"`
	SogouWeight     *int16             `bson:"sogou_weight,omitempty" json:"sogou_weight,omitempty"`
	BingWeight      *int16             `bson:"bing_weight,omitempty" json:"bing_weight,omitempty"`
	So360Weight     *int16             `bson:"so_360_weight,omitempty" json:"so_360_weight,omitempty"`
	ShenmaWeight    *int16             `bson:"shenma_weight,omitempty" json:"shenma_weight,omitempty"`
	PRWeight        *int16             `bson:"pr_weight,omitempty" json:"pr_weight,omitempty"`
	APPPCPCrank     *int64             `bson:"apppc_pc_rank,omitempty" json:"apppc_pc_rank,omitempty"`
	SiteCategory    *string            `bson:"site_category,omitempty" json:"site_category,omitempty"`
	BacklinkCount   *int64             `bson:"backlink_count,omitempty" json:"backlink_count,omitempty"`
	RegistrantName  *string            `bson:"registrant_name,omitempty" json:"registrant_name,omitempty"`
	RegistrantEmail *string            `bson:"registrant_email,omitempty" json:"registrant_email,omitempty"`
	DomainAgeText   *string            `bson:"domain_age_text,omitempty" json:"domain_age_text,omitempty"`
	DomainAgeDays   *int               `bson:"domain_age_days,omitempty" json:"domain_age_days,omitempty"`
	ExpiresOn       *time.Time         `bson:"expires_on,omitempty" json:"expires_on,omitempty"`
	SourceURL       string             `bson:"source_url" json:"source_url"`
	RawSHA256       string             `bson:"raw_sha256" json:"raw_sha256"`
}

type LatestMetric struct {
	Domain Domain  `bson:"domain_record" json:"domain"`
	Metric *Metric `bson:"metric,omitempty" json:"metric,omitempty"`
}

// Certificate is the latest TLS certificate observed for a monitored domain.
// Failed checks are also stored so the UI can distinguish an unreachable host
// from a domain that has not been checked yet.
type Certificate struct {
	ID            primitive.ObjectID `bson:"_id,omitempty" json:"id,omitempty"`
	DomainID      primitive.ObjectID `bson:"domain_id" json:"domain_id"`
	Domain        string             `bson:"domain" json:"domain"`
	Issuer        string             `bson:"issuer,omitempty" json:"issuer,omitempty"`
	Subject       string             `bson:"subject,omitempty" json:"subject,omitempty"`
	SerialNumber  string             `bson:"serial_number,omitempty" json:"serial_number,omitempty"`
	DNSNames      []string           `bson:"dns_names,omitempty" json:"dns_names,omitempty"`
	ValidFrom     *time.Time         `bson:"valid_from,omitempty" json:"valid_from,omitempty"`
	ExpiresAt     *time.Time         `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	CheckedAt     time.Time          `bson:"checked_at" json:"checked_at"`
	HostnameValid bool               `bson:"hostname_valid" json:"hostname_valid"`
	ErrorMessage  *string            `bson:"error_message,omitempty" json:"error_message,omitempty"`
}

type LatestCertificate struct {
	Domain      Domain       `bson:"domain_record" json:"domain"`
	Certificate *Certificate `bson:"certificate,omitempty" json:"certificate,omitempty"`
}

type CertificateSummary struct {
	Total        int64 `bson:"total" json:"total"`
	Checked      int64 `bson:"checked" json:"checked"`
	ExpiringSoon int64 `bson:"expiring_soon" json:"expiring_soon"`
	Expired      int64 `bson:"expired" json:"expired"`
	Failed       int64 `bson:"failed" json:"failed"`
}

type CollectionJob struct {
	ID           primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	DomainID     primitive.ObjectID `bson:"domain_id" json:"domain_id"`
	Domain       string             `bson:"domain" json:"domain,omitempty"`
	SnapshotDate time.Time          `bson:"snapshot_date" json:"snapshot_date"`
	Status       string             `bson:"status" json:"status"`
	RequestedBy  string             `bson:"requested_by" json:"requested_by"`
	AttemptCount int                `bson:"attempt_count" json:"attempt_count"`
	QueuedAt     time.Time          `bson:"queued_at" json:"queued_at"`
	StartedAt    *time.Time         `bson:"started_at,omitempty" json:"started_at,omitempty"`
	FinishedAt   *time.Time         `bson:"finished_at,omitempty" json:"finished_at,omitempty"`
	ErrorMessage *string            `bson:"error_message,omitempty" json:"error_message,omitempty"`
	DedupeKey    *string            `bson:"dedupe_key,omitempty" json:"-"`
}
