package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"seo-monitor/internal/collector"
	"seo-monitor/internal/domainutil"
	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
)

type Server struct {
	store            *store.Store
	certificates     CertificateRefresher
	titles           TitleRefresher
	location         *time.Location
	apiToken         string
	authSessionTTL   time.Duration
	authCookieSecure bool
	loginLimiter     *loginLimiter
	trustedProxies   []netip.Prefix
	allowedOrigins   map[string]struct{}
	logger           *slog.Logger
}

type CertificateRefresher interface {
	RefreshAsync() bool
	Progress() model.TaskProgress
}

type TitleRefresher interface {
	RefreshAsync() bool
	Progress() model.TaskProgress
}

func New(st *store.Store, certificates CertificateRefresher, titles TitleRefresher, location *time.Location, apiToken string, authSessionTTL time.Duration, authCookieSecure bool, loginProtection LoginProtectionConfig, origins []string, logger *slog.Logger) http.Handler {
	server := &Server{
		store: st, certificates: certificates, titles: titles, location: location, apiToken: apiToken, authSessionTTL: authSessionTTL,
		authCookieSecure: authCookieSecure,
		loginLimiter:     newLoginLimiter(loginProtection), trustedProxies: loginProtection.TrustedProxies,
		allowedOrigins: make(map[string]struct{}, len(origins)), logger: logger,
	}
	for _, origin := range origins {
		server.allowedOrigins[origin] = struct{}{}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("POST /api/v1/auth/login", server.login)
	mux.HandleFunc("GET /api/v1/auth/me", server.currentUser)
	mux.HandleFunc("POST /api/v1/auth/logout", server.logout)
	mux.HandleFunc("POST /api/v1/auth/password", server.changePassword)
	mux.HandleFunc("GET /api/v1/domains", server.listDomains)
	mux.HandleFunc("POST /api/v1/domains", server.createDomain)
	mux.HandleFunc("POST /api/v1/domains/bulk", server.bulkDomains)
	mux.HandleFunc("GET /api/v1/search", server.searchLatest)
	mux.HandleFunc("GET /api/v1/certificates", server.listCertificates)
	mux.HandleFunc("POST /api/v1/certificates/refresh", server.refreshCertificates)
	mux.HandleFunc("GET /api/v1/certificates/progress", server.certificateProgress)
	mux.HandleFunc("GET /api/v1/certificates/{id}/history", server.certificateHistory)
	mux.HandleFunc("GET /api/v1/titles", server.listTitles)
	mux.HandleFunc("POST /api/v1/titles/refresh", server.refreshTitles)
	mux.HandleFunc("GET /api/v1/titles/progress", server.titleProgress)
	mux.HandleFunc("GET /api/v1/titles/{id}/history", server.titleHistory)
	mux.HandleFunc("GET /api/v1/domains/{id}", server.getDomain)
	mux.HandleFunc("PATCH /api/v1/domains/{id}", server.updateDomain)
	mux.HandleFunc("DELETE /api/v1/domains/{id}", server.deleteDomain)
	mux.HandleFunc("POST /api/v1/domains/{id}/collect", server.collectDomain)
	mux.HandleFunc("GET /api/v1/domains/{id}/metrics", server.domainMetrics)
	mux.HandleFunc("GET /api/v1/domains/{id}/latest", server.latestMetric)
	mux.HandleFunc("POST /api/v1/collect", server.collectAll)
	mux.HandleFunc("GET /api/v1/collect/progress", server.collectionProgress)
	mux.HandleFunc("GET /api/v1/jobs", server.listJobs)

	return server.recoverPanic(server.logRequests(server.cors(server.authenticate(mux))))
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type authenticatedUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

type authContextKey struct{}

const sessionCookieName = "seo_monitor_session"

const (
	roleAdmin    = "admin"
	roleReadonly = "readonly"
)

func publicUser(user model.User) authenticatedUser {
	return authenticatedUser{ID: user.ID.Hex(), Username: user.Username, Role: user.Role}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request loginRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(strings.TrimSpace(request.Username)) > 100 || len(request.Password) > 72 {
		writeError(w, http.StatusBadRequest, "账号或密码格式无效")
		return
	}
	requestIP := clientIP(r, s.trustedProxies)
	attempt, retryAfter := s.loginLimiter.Start(requestIP, request.Username)
	if attempt == nil {
		s.logger.Warn("login rate limited", "client_ip", requestIP, "retry_after", retryAfter)
		writeLoginRateLimit(w, retryAfter)
		return
	}
	defer attempt.Cancel()
	user, err := s.store.AuthenticateUser(r.Context(), request.Username, request.Password)
	if errors.Is(err, store.ErrNotFound) {
		if retryAfter := attempt.Failure(); retryAfter > 0 {
			s.logger.Warn("login rate limited after failed authentication", "client_ip", requestIP, "retry_after", retryAfter)
			writeLoginRateLimit(w, retryAfter)
			return
		}
		writeError(w, http.StatusUnauthorized, "账号或密码错误")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}
	attempt.Success()
	token, expiresAt, err := s.store.CreateSession(r.Context(), user.ID, s.authSessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建登录会话失败")
		return
	}
	s.setSessionCookie(w, token, expiresAt)
	writeJSON(w, http.StatusOK, map[string]any{
		"expires_at": expiresAt, "user": publicUser(user),
	})
}

func (s *Server) currentUser(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := r.Context().Value(authContextKey{}).(model.User)
	if !ok {
		writeError(w, http.StatusUnauthorized, "请使用账号登录")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": publicUser(user)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.Context().Value(authContextKey{}).(model.User); !ok {
		writeError(w, http.StatusUnauthorized, "请使用账号登录")
		return
	}
	if err := s.store.DeleteSession(r.Context(), sessionToken(r)); err != nil {
		writeError(w, http.StatusInternalServerError, "退出登录失败")
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := r.Context().Value(authContextKey{}).(model.User)
	if !ok {
		writeError(w, http.StatusUnauthorized, "请使用账号登录")
		return
	}
	var request changePasswordRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(request.CurrentPassword) > 72 || len(request.NewPassword) < 12 || len(request.NewPassword) > 72 {
		writeError(w, http.StatusBadRequest, "新密码必须为 12 到 72 字节")
		return
	}
	if request.CurrentPassword == request.NewPassword {
		writeError(w, http.StatusBadRequest, "新密码不能与当前密码相同")
		return
	}
	if err := s.store.ChangePassword(r.Context(), user.ID, request.CurrentPassword, request.NewPassword); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "当前密码错误")
		return
	} else if errors.Is(err, store.ErrInvalidPassword) {
		writeError(w, http.StatusBadRequest, "新密码必须为 12 到 72 字节且不能包含换行符")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "修改密码失败，请稍后重试")
		return
	}
	s.clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Health(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "MongoDB unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createDomainRequest struct {
	Domain      string  `json:"domain"`
	DisplayName *string `json:"display_name"`
}

func (s *Server) createDomain(w http.ResponseWriter, r *http.Request) {
	var request createDomainRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	domain, err := domainutil.Normalize(request.Domain)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.DisplayName = cleanOptionalString(request.DisplayName)
	item, err := s.store.CreateDomain(r.Context(), domain, request.DisplayName)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "域名已存在")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "创建域名失败")
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("include_archived") == "true"
	items, err := s.store.ListDomains(r.Context(), includeArchived)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询域名失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	item, err := s.store.GetDomain(r.Context(), id)
	if !handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

type updateDomainRequest struct {
	DisplayName *string `json:"display_name"`
	Active      *bool   `json:"active"`
}

func (s *Server) updateDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	var request updateDomainRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	patch := model.DomainPatch{}
	if request.DisplayName != nil {
		patch.HasDisplayName = true
		patch.DisplayName = strings.TrimSpace(*request.DisplayName)
	}
	if request.Active != nil {
		patch.HasActive = true
		patch.Active = *request.Active
	}
	item, err := s.store.UpdateDomain(r.Context(), id, patch)
	if !handleStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	if !handleStoreError(w, s.store.ArchiveDomain(r.Context(), id)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type bulkDomainRequest struct {
	Domains []string `json:"domains"`
}

func (s *Server) bulkDomains(w http.ResponseWriter, r *http.Request) {
	var request bulkDomainRequest
	if err := decodeJSON(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(request.Domains) == 0 || len(request.Domains) > 1000 {
		writeError(w, http.StatusBadRequest, "domains 数量必须在 1 到 1000 之间")
		return
	}
	type rejected struct {
		Input  string `json:"input"`
		Reason string `json:"reason"`
	}
	created := make([]model.Domain, 0, len(request.Domains))
	skipped := make([]string, 0)
	rejectedItems := make([]rejected, 0)
	seen := map[string]struct{}{}
	for _, input := range request.Domains {
		domain, err := domainutil.Normalize(input)
		if err != nil {
			rejectedItems = append(rejectedItems, rejected{input, err.Error()})
			continue
		}
		if _, exists := seen[domain]; exists {
			skipped = append(skipped, domain)
			continue
		}
		seen[domain] = struct{}{}
		item, err := s.store.CreateDomain(r.Context(), domain, nil)
		if errors.Is(err, store.ErrConflict) {
			skipped = append(skipped, domain)
			continue
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "批量创建中断")
			return
		}
		created = append(created, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "skipped": skipped, "rejected": rejectedItems})
}

type collectRequest struct {
	Force bool `json:"force"`
}

func (s *Server) collectDomain(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	request := collectRequest{}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	requestedBy := "manual"
	if request.Force {
		requestedBy = "manual-force"
	}
	date := collector.SnapshotDate(time.Now(), s.location)
	job, added, err := s.store.QueueDomain(r.Context(), id, date, requestedBy, request.Force)
	if !handleStoreError(w, err) {
		return
	}
	if !added {
		writeJSON(w, http.StatusOK, map[string]any{"queued": false, "message": "当天已有成功结果或任务正在执行"})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": true, "job": job})
}

func (s *Server) collectAll(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSON(w, r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	requestedBy := "manual"
	if request.Force {
		requestedBy = "manual-force"
	}
	date := collector.SnapshotDate(time.Now(), s.location)
	count, err := s.store.QueueAll(r.Context(), date, requestedBy, request.Force)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "批量排队失败")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": count, "snapshot_date": date})
}

func (s *Server) collectionProgress(w http.ResponseWriter, r *http.Request) {
	date := collector.SnapshotDate(time.Now(), s.location)
	progress, err := s.store.CollectionProgress(r.Context(), date)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询采集进度失败")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func (s *Server) domainMetrics(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetDomain(r.Context(), id); !handleStoreError(w, err) {
		return
	}
	today := collector.SnapshotDate(time.Now(), s.location)
	from, err := queryDate(r, "from", today.AddDate(0, 0, -89))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	to, err := queryDate(r, "to", today)
	if err != nil || to.Before(from) {
		writeError(w, http.StatusBadRequest, "to 必须是有效日期且不早于 from")
		return
	}
	items, err := s.store.Metrics(r.Context(), id, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询趋势失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items), "from": from, "to": to})
}

func (s *Server) latestMetric(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	if _, err := s.store.GetDomain(r.Context(), id); !handleStoreError(w, err) {
		return
	}
	metric, err := s.store.LatestMetric(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询最新数据失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"metric": metric})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := int64(100)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 1000 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 1000 之间")
			return
		}
		limit = parsed
	}
	items, err := s.store.ListJobs(r.Context(), r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询任务失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func (s *Server) searchLatest(w http.ResponseWriter, r *http.Request) {
	field := strings.TrimSpace(r.URL.Query().Get("field"))
	if field == "" {
		field = "domain"
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	sortField := strings.TrimSpace(r.URL.Query().Get("sort_by"))
	sortOrder := strings.TrimSpace(r.URL.Query().Get("sort_order"))
	page := int64(1)
	limit := int64(50)
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "page 必须为正整数")
			return
		}
		page = parsed
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 100 之间")
			return
		}
		limit = parsed
	}

	items, total, err := s.store.SearchLatest(r.Context(), field, query, status, sortField, sortOrder, page, limit)
	if errors.Is(err, store.ErrInvalidSearch) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "搜索最新指标失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items), "total": total,
		"page": page, "limit": limit, "field": field, "q": query, "status": status,
		"sort_by": sortField, "sort_order": sortOrder,
	})
}

func (s *Server) listCertificates(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	page := int64(1)
	limit := int64(50)
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "page must be a positive integer")
			return
		}
		page = parsed
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}

	items, total, summary, err := s.store.ListCertificates(r.Context(), query, status, page, limit)
	if errors.Is(err, store.ErrInvalidSearch) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询证书信息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items), "total": total,
		"page": page, "limit": limit, "q": query, "status": status,
		"summary": summary,
	})
}

func (s *Server) refreshCertificates(w http.ResponseWriter, _ *http.Request) {
	if s.certificates == nil {
		writeError(w, http.StatusServiceUnavailable, "证书检测服务未启用")
		return
	}
	if !s.certificates.RefreshAsync() {
		writeJSON(w, http.StatusOK, map[string]any{
			"started": false, "message": "证书检测任务正在执行", "progress": s.certificates.Progress(),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started": true, "message": "证书检测任务已启动", "progress": s.certificates.Progress(),
	})
}

func (s *Server) certificateProgress(w http.ResponseWriter, _ *http.Request) {
	if s.certificates == nil {
		writeError(w, http.StatusServiceUnavailable, "证书检测服务未启用")
		return
	}
	writeJSON(w, http.StatusOK, s.certificates.Progress())
}

func (s *Server) certificateHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	limit := int64(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	items, err := s.store.CertificateHistory(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询证书轮询记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items,
		"count": len(items),
	})
}

func (s *Server) listTitles(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	page := int64(1)
	limit := int64(50)
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 {
			writeError(w, http.StatusBadRequest, "page 必须为正整数")
			return
		}
		page = parsed
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 100 之间")
			return
		}
		limit = parsed
	}
	items, total, summary, err := s.store.ListDomainTitles(r.Context(), query, status, page, limit)
	if errors.Is(err, store.ErrInvalidSearch) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询标题信息失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "count": len(items), "total": total,
		"page": page, "limit": limit, "q": query, "status": status, "summary": summary,
	})
}

func (s *Server) refreshTitles(w http.ResponseWriter, _ *http.Request) {
	if s.titles == nil {
		writeError(w, http.StatusServiceUnavailable, "标题检测服务未启用")
		return
	}
	if !s.titles.RefreshAsync() {
		writeJSON(w, http.StatusOK, map[string]any{
			"started": false, "message": "标题检测任务正在执行", "progress": s.titles.Progress(),
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started": true, "message": "标题检测任务已启动", "progress": s.titles.Progress(),
	})
}

func (s *Server) titleProgress(w http.ResponseWriter, _ *http.Request) {
	if s.titles == nil {
		writeError(w, http.StatusServiceUnavailable, "标题检测服务未启用")
		return
	}
	writeJSON(w, http.StatusOK, s.titles.Progress())
}

func (s *Server) titleHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := objectID(w, r)
	if !ok {
		return
	}
	limit := int64(50)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "limit 必须在 1 到 100 之间")
			return
		}
		limit = parsed
	}
	items, err := s.store.DomainTitleHistory(r.Context(), id, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "查询标题变更记录失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
}

func objectID(w http.ResponseWriter, r *http.Request) (primitive.ObjectID, bool) {
	id, err := primitive.ObjectIDFromHex(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的域名 ID")
		return primitive.NilObjectID, false
	}
	return id, true
}

func queryDate(r *http.Request, key string, fallback time.Time) (time.Time, error) {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s 必须为 YYYY-MM-DD", key)
	}
	return parsed.UTC(), nil
}

func handleStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "记录不存在")
		return false
	}
	writeError(w, http.StatusInternalServerError, "数据库操作失败")
	return false
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("无效 JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("请求体只能包含一个 JSON 对象")
	}
	return nil
}

func cleanOptionalString(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodOptions || r.URL.Path == "/api/v1/auth/login" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		token := bearerToken(r)
		if s.apiToken != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.apiToken)) == 1 {
			next.ServeHTTP(w, r)
			return
		}
		cookieAuthenticated := false
		if token == "" {
			if cookie, cookieErr := r.Cookie(sessionCookieName); cookieErr == nil {
				token = cookie.Value
				cookieAuthenticated = true
			}
		}
		user, err := s.store.AuthenticateSession(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "登录已失效，请重新登录")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "验证登录状态失败")
			return
		}
		if !roleCanAccessRequest(user.Role, r.Method, r.URL.Path) {
			s.logger.Warn("authenticated account is not allowed to access request", "user_id", user.ID.Hex(), "role", user.Role, "method", r.Method, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "账号没有权限执行此操作")
			return
		}
		if cookieAuthenticated && requestChangesState(r.Method) && r.Header.Get("X-CSRF-Protection") != "1" {
			writeError(w, http.StatusForbidden, "请求缺少 CSRF 防护标记")
			return
		}
		ctx := context.WithValue(r.Context(), authContextKey{}, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
}

func sessionToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func requestChangesState(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func roleCanAccessRequest(role, method, path string) bool {
	if role == roleAdmin {
		return true
	}
	if role != roleReadonly {
		return false
	}
	if !requestChangesState(method) {
		return true
	}
	return method == http.MethodPost && path == "/api/v1/auth/logout"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: token, Path: "/api/", HttpOnly: true,
		Secure: s.authCookieSecure, SameSite: http.SameSiteStrictMode,
		Expires: expiresAt, MaxAge: int(time.Until(expiresAt).Seconds()),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/api/", HttpOnly: true,
		Secure: s.authCookieSecure, SameSite: http.SameSiteStrictMode,
		Expires: time.Unix(1, 0), MaxAge: -1,
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if _, allowed := s.allowedOrigins[origin]; origin != "" && allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Protection")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		wrapped := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "status", wrapped.status, "duration", time.Since(started))
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("http panic", "value", recovered)
				writeError(w, http.StatusInternalServerError, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
