// Package quota provides quota management functionality for various AI providers.
// It includes Codex quota caching and routing based on quota availability.
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

const (
	// DefaultCodexQuotaRefreshInterval is the default interval for refreshing Codex quota (30 minutes).
	DefaultCodexQuotaRefreshInterval = 30 * time.Minute

	// CodexQuotaCacheExpiry is the default expiry duration for quota cache.
	CodexQuotaCacheExpiry = 35 * time.Minute

	// CodexUsageEndpoint is the default endpoint for querying Codex quota.
	CodexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"

	// CodexQuotaMetadataKey is the auth metadata key used for persisted codex quota cache.
	CodexQuotaMetadataKey = "codex_quota_cache"
)

type persistedCodexQuota struct {
	QuotaInfo   *CodexQuotaInfo `json:"quota_info"`
	FetchedAt   time.Time       `json:"fetched_at"`
	ExpiresAt   time.Time       `json:"expires_at"`
	AccountID   string          `json:"account_id,omitempty"`
	AccessToken string          `json:"access_token,omitempty"`
}

// CodexQuotaInfo represents the quota information returned by the Codex usage endpoint.
type CodexQuotaInfo struct {
	UserID    string `json:"user_id"`
	AccountID string `json:"account_id"`
	Email     string `json:"email"`
	PlanType  string `json:"plan_type"`

	// RateLimit is the primary rate limit for general usage.
	RateLimit *RateLimitInfo `json:"rate_limit,omitempty"`

	// CodeReviewRateLimit is the rate limit for code review features.
	CodeReviewRateLimit *RateLimitInfo `json:"code_review_rate_limit,omitempty"`

	// Promo contains promotional information (e.g., free trial offers).
	Promo *PromoInfo `json:"promo,omitempty"`
}

// RateLimitInfo represents rate limit details for a specific window.
type RateLimitInfo struct {
	Allowed         bool         `json:"allowed"`
	LimitReached    bool         `json:"limit_reached"`
	PrimaryWindow   *LimitWindow `json:"primary_window,omitempty"`
	SecondaryWindow *LimitWindow `json:"secondary_window,omitempty"`
}

// LimitWindow represents a rate limit window.
type LimitWindow struct {
	UsedPercent        int   `json:"used_percent"`
	LimitWindowSeconds int64 `json:"limit_window_seconds"`
	ResetAfterSeconds  int64 `json:"reset_after_seconds"`
	ResetAt            int64 `json:"reset_at"` // Unix timestamp
}

// PromoInfo contains promotional campaign information.
type PromoInfo struct {
	CampaignID string `json:"campaign_id"`
	Message    string `json:"message"`
}

// CodexQuotaCacheEntry holds cached quota data with metadata.
type CodexQuotaCacheEntry struct {
	QuotaInfo   *CodexQuotaInfo
	FetchedAt   time.Time
	ExpiresAt   time.Time
	AccountID   string
	AccessToken string
}

// IsExpired checks if the cache entry has expired.
func (e *CodexQuotaCacheEntry) IsExpired() bool {
	return time.Now().After(e.ExpiresAt)
}

// CodexQuotaManager manages Codex quota caching and refreshing.
type CodexQuotaManager struct {
	mu       sync.RWMutex
	cache    map[string]*CodexQuotaCacheEntry // keyed by account_id or auth_index
	interval time.Duration
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewCodexQuotaManager creates a new Codex quota manager.
func NewCodexQuotaManager(interval time.Duration) *CodexQuotaManager {
	if interval <= 0 {
		interval = DefaultCodexQuotaRefreshInterval
	}
	return &CodexQuotaManager{
		cache:    make(map[string]*CodexQuotaCacheEntry),
		interval: interval,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the periodic quota refresh goroutine.
func (m *CodexQuotaManager) Start(ctx context.Context, fetcher QuotaFetcher) {
	if m.interval <= 0 {
		log.Info("Codex quota auto-refresh is disabled (interval <= 0)")
		return
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()

		log.Infof("Codex quota auto-refresh started with interval %v", m.interval)

		// Perform initial fetch
		if fetcher != nil {
			m.fetchAllQuotas(ctx, fetcher)
		}

		for {
			select {
			case <-ctx.Done():
				log.Info("Codex quota auto-refresh stopped (context cancelled)")
				return
			case <-m.stopCh:
				log.Info("Codex quota auto-refresh stopped")
				return
			case <-ticker.C:
				if fetcher != nil {
					m.fetchAllQuotas(ctx, fetcher)
				}
			}
		}
	}()
}

// Stop stops the periodic quota refresh goroutine.
func (m *CodexQuotaManager) Stop() {
	close(m.stopCh)
	m.wg.Wait()
}

// QuotaFetcher defines the interface for fetching quota data.
type QuotaFetcher interface {
	// ListCodexAuths returns a list of Codex authentication entries.
	ListCodexAuths() []CodexAuthEntry

	// FetchQuota fetches quota for a single auth entry.
	FetchQuota(ctx context.Context, auth CodexAuthEntry) (*CodexQuotaInfo, error)
}

// CodexAuthEntry represents a Codex authentication entry.
type CodexAuthEntry struct {
	AuthIndex   string
	AccountID   string
	AccessToken string
	ProxyURL    string
}

// fetchAllQuotas fetches quota for all Codex auth entries.
func (m *CodexQuotaManager) fetchAllQuotas(ctx context.Context, fetcher QuotaFetcher) {
	auths := fetcher.ListCodexAuths()
	log.Debugf("Codex quota refresh: found %d Codex auth entries", len(auths))

	for _, auth := range auths {
		if auth.AccessToken == "" {
			continue
		}

		quotaInfo, err := fetcher.FetchQuota(ctx, auth)
		if err != nil {
			log.WithError(err).Debugf("Codex quota fetch failed for auth %s", auth.AuthIndex)
			continue
		}

		m.UpdateCache(auth.AuthIndex, auth.AccountID, quotaInfo, auth.AccessToken)
		log.Debugf("Codex quota refreshed for auth %s (account: %s, plan: %s, used: %d%%)",
			auth.AuthIndex, auth.AccountID, quotaInfo.PlanType,
			getUsedPercent(quotaInfo))
	}
}

func getUsedPercent(info *CodexQuotaInfo) int {
	if info == nil || info.RateLimit == nil || info.RateLimit.PrimaryWindow == nil {
		return -1
	}
	return info.RateLimit.PrimaryWindow.UsedPercent
}

// UpdateCache updates the quota cache for a given auth index.
func (m *CodexQuotaManager) UpdateCache(authIndex, accountID string, quotaInfo *CodexQuotaInfo, accessToken string) {
	if quotaInfo == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.cache[authIndex] = &CodexQuotaCacheEntry{
		QuotaInfo:   quotaInfo,
		FetchedAt:   now,
		ExpiresAt:   now.Add(CodexQuotaCacheExpiry),
		AccountID:   accountID,
		AccessToken: accessToken,
	}
}

// GetQuota retrieves cached quota for a given auth index.
func (m *CodexQuotaManager) GetQuota(authIndex string) *CodexQuotaCacheEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entry, exists := m.cache[authIndex]
	if !exists {
		return nil
	}

	return entry
}

// GetQuotaInfo retrieves the quota info for a given auth index.
func (m *CodexQuotaManager) GetQuotaInfo(authIndex string) *CodexQuotaInfo {
	entry := m.GetQuota(authIndex)
	if entry == nil {
		return nil
	}
	return entry.QuotaInfo
}

// RemoveQuota removes cached quota for a given auth index.
func (m *CodexQuotaManager) RemoveQuota(authIndex string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.cache, authIndex)
}

// ClearCache clears all cached quota data.
func (m *CodexQuotaManager) ClearCache() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache = make(map[string]*CodexQuotaCacheEntry)
}

// RestoreCache restores a cache entry for a given auth index when the entry is still valid.
func (m *CodexQuotaManager) RestoreCache(authIndex string, entry *CodexQuotaCacheEntry) bool {
	authIndex = strings.TrimSpace(authIndex)
	if authIndex == "" || entry == nil || entry.QuotaInfo == nil {
		return false
	}

	if entry.ExpiresAt.IsZero() {
		entry.ExpiresAt = entry.FetchedAt.Add(CodexQuotaCacheExpiry)
	}
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if current, exists := m.cache[authIndex]; exists && current != nil {
		if !current.FetchedAt.IsZero() && current.FetchedAt.After(entry.FetchedAt) {
			return false
		}
	}

	m.cache[authIndex] = &CodexQuotaCacheEntry{
		QuotaInfo:   entry.QuotaInfo,
		FetchedAt:   entry.FetchedAt,
		ExpiresAt:   entry.ExpiresAt,
		AccountID:   entry.AccountID,
		AccessToken: entry.AccessToken,
	}
	return true
}

// PersistQuotaToMetadata writes codex quota cache entry into auth metadata.
func PersistQuotaToMetadata(metadata map[string]any, entry *CodexQuotaCacheEntry) map[string]any {
	if entry == nil || entry.QuotaInfo == nil {
		return metadata
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	payload := persistedCodexQuota{
		QuotaInfo:   entry.QuotaInfo,
		FetchedAt:   entry.FetchedAt,
		ExpiresAt:   entry.ExpiresAt,
		AccountID:   strings.TrimSpace(entry.AccountID),
		AccessToken: strings.TrimSpace(entry.AccessToken),
	}
	if payload.FetchedAt.IsZero() {
		payload.FetchedAt = time.Now()
	}
	if payload.ExpiresAt.IsZero() {
		payload.ExpiresAt = payload.FetchedAt.Add(CodexQuotaCacheExpiry)
	}
	metadata[CodexQuotaMetadataKey] = payload
	return metadata
}

// ReadQuotaFromMetadata extracts codex quota cache entry from auth metadata.
func ReadQuotaFromMetadata(metadata map[string]any) (*CodexQuotaCacheEntry, bool) {
	if len(metadata) == 0 {
		return nil, false
	}
	raw, ok := metadata[CodexQuotaMetadataKey]
	if !ok || raw == nil {
		return nil, false
	}

	encoded, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil, false
	}
	var payload persistedCodexQuota
	if errUnmarshal := json.Unmarshal(encoded, &payload); errUnmarshal != nil {
		return nil, false
	}
	if payload.QuotaInfo == nil {
		return nil, false
	}
	if payload.ExpiresAt.IsZero() {
		if payload.FetchedAt.IsZero() {
			payload.FetchedAt = time.Now()
		}
		payload.ExpiresAt = payload.FetchedAt.Add(CodexQuotaCacheExpiry)
	}
	return &CodexQuotaCacheEntry{
		QuotaInfo:   payload.QuotaInfo,
		FetchedAt:   payload.FetchedAt,
		ExpiresAt:   payload.ExpiresAt,
		AccountID:   strings.TrimSpace(payload.AccountID),
		AccessToken: strings.TrimSpace(payload.AccessToken),
	}, true
}

// SelectBestAuthByQuota selects the best auth index based on quota priority.
// Priority order:
// 1. !limit_reached (allowed to make requests)
// 2. Lower used_percent (less usage)
// 3. Earlier reset_at (sooner quota reset)
func (m *CodexQuotaManager) SelectBestAuthByQuota(authIndexes []string) string {
	if len(authIndexes) == 0 {
		return ""
	}

	if len(authIndexes) == 1 {
		return authIndexes[0]
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	var bestAuth string
	var bestQuota *CodexQuotaInfo

	for _, authIndex := range authIndexes {
		entry, exists := m.cache[authIndex]
		if !exists || entry.QuotaInfo == nil {
			continue
		}

		quota := entry.QuotaInfo
		if quota.RateLimit == nil || !quota.RateLimit.Allowed {
			continue
		}

		if bestQuota == nil {
			bestAuth = authIndex
			bestQuota = quota
			continue
		}

		// Compare: prefer !limit_reached
		if !quota.RateLimit.LimitReached && bestQuota.RateLimit.LimitReached {
			bestAuth = authIndex
			bestQuota = quota
			continue
		}
		if quota.RateLimit.LimitReached && !bestQuota.RateLimit.LimitReached {
			continue
		}

		// Compare: prefer lower used_percent
		usedPercent := getUsedPercent(quota)
		bestUsedPercent := getUsedPercent(bestQuota)

		if usedPercent >= 0 && bestUsedPercent >= 0 {
			if usedPercent < bestUsedPercent {
				bestAuth = authIndex
				bestQuota = quota
				continue
			}
			if usedPercent > bestUsedPercent {
				continue
			}
		}

		// Compare: prefer earlier reset_at
		if quota.RateLimit.PrimaryWindow != nil && bestQuota.RateLimit.PrimaryWindow != nil {
			if quota.RateLimit.PrimaryWindow.ResetAt < bestQuota.RateLimit.PrimaryWindow.ResetAt {
				bestAuth = authIndex
				bestQuota = quota
			}
		}
	}

	if bestAuth == "" && len(authIndexes) > 0 {
		return authIndexes[0]
	}

	return bestAuth
}

// FetchQuotaForAuth fetches quota for a single auth entry.
func FetchQuotaForAuth(ctx context.Context, accessToken, accountID, proxyURL string) (*CodexQuotaInfo, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("access token is empty")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CodexUsageEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildTransport(proxyURL),
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var quotaInfo CodexQuotaInfo
	if err := json.NewDecoder(resp.Body).Decode(&quotaInfo); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &quotaInfo, nil
}

func buildTransport(proxyURL string) http.RoundTripper {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return http.DefaultTransport
	}

	parsedURL, err := url.Parse(proxyURL)
	if err != nil {
		log.WithError(err).Debug("parse proxy URL failed")
		return http.DefaultTransport
	}

	if parsedURL.Scheme == "socks5" {
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.WithError(errSOCKS5).Debug("create SOCKS5 dialer failed")
			return http.DefaultTransport
		}
		return &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}
	}

	if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		return &http.Transport{Proxy: http.ProxyURL(parsedURL)}
	}

	log.Debugf("unsupported proxy scheme: %s", parsedURL.Scheme)
	return http.DefaultTransport
}
