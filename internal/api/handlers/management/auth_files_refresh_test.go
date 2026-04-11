package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestListAuthFiles_RefreshesPaidCodexQuotaAsynchronously(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authPath := filepath.Join(tempDir, "codex.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		FileName: "codex.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"access_token": "token-1",
			"account_id":   "acct-1",
		},
	}
	auth.Metadata = quota.PersistQuotaToMetadata(auth.Metadata, &quota.CodexQuotaCacheEntry{
		QuotaInfo: &quota.CodexQuotaInfo{
			AccountID: "acct-1",
			Email:     "cached@example.com",
			PlanType:  "plus",
			RateLimit: &quota.RateLimitInfo{
				Allowed: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent: 25,
				},
			},
		},
		FetchedAt:   time.Now().Add(-authFilesCodexQuotaRefreshCooldown - time.Second),
		ExpiresAt:   time.Now().Add(5 * time.Minute),
		AccountID:   "acct-1",
		AccessToken: "token-1",
	})
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	var fetchCalls atomic.Int32
	fetchStarted := make(chan struct{}, 1)
	releaseFetch := make(chan struct{})
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls.Add(1)
		if accessToken != "token-1" {
			t.Fatalf("access token = %q, want token-1", accessToken)
		}
		if accountID != "acct-1" {
			t.Fatalf("account id = %q, want acct-1", accountID)
		}
		select {
		case fetchStarted <- struct{}{}:
		default:
		}
		<-releaseFetch
		return &quota.CodexQuotaInfo{
			AccountID: "acct-1",
			Email:     "user@example.com",
			PlanType:  "plus",
			RateLimit: &quota.RateLimitInfo{
				Allowed:      false,
				LimitReached: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent:        100,
					LimitWindowSeconds: 604800,
					ResetAfterSeconds:  551955,
					ResetAt:            1776163127,
				},
			},
		}, nil
	}
	t.Cleanup(func() {
		fetchCodexQuotaForAuth = originalFetcher
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	done := make(chan struct{})
	go func() {
		h.ListAuthFiles(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		close(releaseFetch)
		t.Fatal("ListAuthFiles blocked waiting for quota refresh")
	}

	if rec.Code != http.StatusOK {
		close(releaseFetch)
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	select {
	case <-fetchStarted:
	case <-time.After(time.Second):
		close(releaseFetch)
		t.Fatal("expected background quota refresh to start")
	}

	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			AuthIndex  string `json:"auth_index"`
			CodexQuota struct {
				Email     string `json:"email"`
				PlanType  string `json:"plan_type"`
				RateLimit struct {
					Allowed       bool `json:"allowed"`
					LimitReached  bool `json:"limit_reached"`
					PrimaryWindow struct {
						UsedPercent int `json:"used_percent"`
					} `json:"primary_window"`
				} `json:"rate_limit"`
			} `json:"codex_quota"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	file := payload.Files[0]
	if file.Name != "codex.json" {
		t.Fatalf("file name = %q, want codex.json", file.Name)
	}
	if file.AuthIndex != registered.Index {
		t.Fatalf("auth index = %q, want %q", file.AuthIndex, registered.Index)
	}
	if file.CodexQuota.Email != "cached@example.com" {
		close(releaseFetch)
		t.Fatalf("quota email = %q, want cached@example.com", file.CodexQuota.Email)
	}
	if file.CodexQuota.PlanType != "plus" {
		t.Fatalf("quota plan_type = %q, want plus", file.CodexQuota.PlanType)
	}
	if file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent != 25 {
		close(releaseFetch)
		t.Fatalf("used_percent = %d, want 25", file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent)
	}

	close(releaseFetch)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		updated := h.authByIndex(registered.Index)
		if updated != nil {
			entry, ok := quota.ReadQuotaFromMetadata(updated.Metadata)
			if ok && entry != nil && entry.QuotaInfo != nil && entry.QuotaInfo.RateLimit != nil && entry.QuotaInfo.RateLimit.PrimaryWindow != nil && entry.QuotaInfo.RateLimit.PrimaryWindow.UsedPercent == 100 {
				if fetchCalls.Load() != 1 {
					t.Fatalf("fetch calls = %d, want 1", fetchCalls.Load())
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("expected quota metadata to be updated asynchronously")
}

func TestListAuthFiles_SkipsFreeCodexQuotaRefresh(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authPath := filepath.Join(tempDir, "codex-free.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-free-auth",
		FileName: "codex-free.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path":      authPath,
			"plan_type": "free",
		},
		Metadata: map[string]any{
			"access_token": "token-free",
			"account_id":   "acct-free",
			"plan_type":    "free",
		},
	}
	auth.Metadata = quota.PersistQuotaToMetadata(auth.Metadata, &quota.CodexQuotaCacheEntry{
		QuotaInfo: &quota.CodexQuotaInfo{
			AccountID: "acct-free",
			Email:     "free@example.com",
			PlanType:  "free",
			RateLimit: &quota.RateLimitInfo{
				Allowed: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent: 10,
				},
			},
		},
		FetchedAt:   time.Now().Add(-authFilesCodexQuotaRefreshCooldown - time.Second),
		ExpiresAt:   time.Now().Add(5 * time.Minute),
		AccountID:   "acct-free",
		AccessToken: "token-free",
	})
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	var fetchCalls atomic.Int32
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls.Add(1)
		return &quota.CodexQuotaInfo{PlanType: "free"}, nil
	}
	t.Cleanup(func() {
		fetchCodexQuotaForAuth = originalFetcher
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fetchCalls.Load() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetchCalls.Load())
	}

	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			CodexQuota struct {
				Email    string `json:"email"`
				PlanType string `json:"plan_type"`
			} `json:"codex_quota"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	if payload.Files[0].Name != "codex-free.json" {
		t.Fatalf("file name = %q, want codex-free.json", payload.Files[0].Name)
	}
	if payload.Files[0].CodexQuota.Email != "free@example.com" {
		t.Fatalf("quota email = %q, want free@example.com", payload.Files[0].CodexQuota.Email)
	}
	if payload.Files[0].CodexQuota.PlanType != "free" {
		t.Fatalf("quota plan_type = %q, want free", payload.Files[0].CodexQuota.PlanType)
	}
}

func TestListAuthFiles_SkipsUnknownCodexQuotaRefresh(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authPath := filepath.Join(tempDir, "codex-unknown.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-unknown-auth",
		FileName: "codex-unknown.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"access_token": "token-unknown",
			"account_id":   "acct-unknown",
		},
	}
	auth.Metadata = quota.PersistQuotaToMetadata(auth.Metadata, &quota.CodexQuotaCacheEntry{
		QuotaInfo: &quota.CodexQuotaInfo{
			AccountID: "acct-unknown",
			Email:     "unknown@example.com",
			PlanType:  "",
			RateLimit: &quota.RateLimitInfo{
				Allowed: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent: 42,
				},
			},
		},
		FetchedAt:   time.Now().Add(-authFilesCodexQuotaRefreshCooldown - time.Second),
		ExpiresAt:   time.Now().Add(5 * time.Minute),
		AccountID:   "acct-unknown",
		AccessToken: "token-unknown",
	})
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	var fetchCalls atomic.Int32
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls.Add(1)
		return &quota.CodexQuotaInfo{PlanType: "plus"}, nil
	}
	t.Cleanup(func() {
		fetchCodexQuotaForAuth = originalFetcher
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ctx.Request.Header.Set("Referer", "http://localhost:8317/management.html#/auth-files")

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fetchCalls.Load() != 0 {
		t.Fatalf("fetch calls = %d, want 0", fetchCalls.Load())
	}

	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			CodexQuota struct {
				Email    string `json:"email"`
				PlanType string `json:"plan_type"`
			} `json:"codex_quota"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	if payload.Files[0].Name != "codex-unknown.json" {
		t.Fatalf("file name = %q, want codex-unknown.json", payload.Files[0].Name)
	}
	if payload.Files[0].CodexQuota.Email != "unknown@example.com" {
		t.Fatalf("quota email = %q, want unknown@example.com", payload.Files[0].CodexQuota.Email)
	}
	if payload.Files[0].CodexQuota.PlanType != "" {
		t.Fatalf("quota plan_type = %q, want empty", payload.Files[0].CodexQuota.PlanType)
	}
}

func TestListAuthFiles_WebUIRefererRefreshesPaidCodexQuotaSynchronously(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	authPath := filepath.Join(tempDir, "codex-plus.json")
	if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-plus-auth",
		FileName: "codex-plus.json",
		Provider: "codex",
		Attributes: map[string]string{
			"path": authPath,
		},
		Metadata: map[string]any{
			"access_token": "token-plus",
			"account_id":   "acct-plus",
		},
	}
	auth.Metadata = quota.PersistQuotaToMetadata(auth.Metadata, &quota.CodexQuotaCacheEntry{
		QuotaInfo: &quota.CodexQuotaInfo{
			AccountID: "acct-plus",
			Email:     "stale@example.com",
			PlanType:  "plus",
			RateLimit: &quota.RateLimitInfo{
				Allowed: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent: 12,
				},
			},
		},
		FetchedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(5 * time.Minute),
		AccountID:   "acct-plus",
		AccessToken: "token-plus",
	})
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	var fetchCalls atomic.Int32
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls.Add(1)
		if accessToken != "token-plus" {
			t.Fatalf("access token = %q, want token-plus", accessToken)
		}
		if accountID != "acct-plus" {
			t.Fatalf("account id = %q, want acct-plus", accountID)
		}
		return &quota.CodexQuotaInfo{
			AccountID: "acct-plus",
			Email:     "fresh@example.com",
			PlanType:  "plus",
			RateLimit: &quota.RateLimitInfo{
				Allowed:      false,
				LimitReached: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent:        100,
					LimitWindowSeconds: 604800,
					ResetAfterSeconds:  3600,
					ResetAt:            1776163127,
				},
			},
		}, nil
	}
	t.Cleanup(func() {
		fetchCodexQuotaForAuth = originalFetcher
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ctx.Request.Header.Set("Referer", "http://localhost:8317/management.html#/auth-files")

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fetchCalls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetchCalls.Load())
	}

	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			AuthIndex  string `json:"auth_index"`
			CodexQuota struct {
				Email     string `json:"email"`
				PlanType  string `json:"plan_type"`
				FetchedAt string `json:"fetched_at"`
				RateLimit struct {
					Allowed       bool `json:"allowed"`
					LimitReached  bool `json:"limit_reached"`
					PrimaryWindow struct {
						UsedPercent int `json:"used_percent"`
					} `json:"primary_window"`
				} `json:"rate_limit"`
			} `json:"codex_quota"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("files len = %d, want 1", len(payload.Files))
	}
	file := payload.Files[0]
	if file.Name != "codex-plus.json" {
		t.Fatalf("file name = %q, want codex-plus.json", file.Name)
	}
	if file.AuthIndex != registered.Index {
		t.Fatalf("auth index = %q, want %q", file.AuthIndex, registered.Index)
	}
	if file.CodexQuota.Email != "fresh@example.com" {
		t.Fatalf("quota email = %q, want fresh@example.com", file.CodexQuota.Email)
	}
	if file.CodexQuota.PlanType != "plus" {
		t.Fatalf("quota plan_type = %q, want plus", file.CodexQuota.PlanType)
	}
	if file.CodexQuota.FetchedAt == "" {
		t.Fatal("expected fetched_at to be populated")
	}
	if file.CodexQuota.RateLimit.Allowed {
		t.Fatal("expected allowed to be false after refresh")
	}
	if !file.CodexQuota.RateLimit.LimitReached {
		t.Fatal("expected limit_reached to be true after refresh")
	}
	if file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent != 100 {
		t.Fatalf("used_percent = %d, want 100", file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent)
	}

	updated := h.authByIndex(registered.Index)
	if updated == nil {
		t.Fatal("expected auth to remain addressable by index")
	}
	entry, ok := quota.ReadQuotaFromMetadata(updated.Metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil {
		t.Fatal("expected refreshed quota metadata to be persisted")
	}
	if entry.QuotaInfo.Email != "fresh@example.com" {
		t.Fatalf("persisted quota email = %q, want fresh@example.com", entry.QuotaInfo.Email)
	}
}

func TestListAuthFiles_WebUIRefererRefreshesLargePaidCodexSetSynchronously(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	tempDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	expectedEmails := make(map[string]string)
	totalAuths := authFilesCodexQuotaWebUIRefreshConcurrency + 9

	for i := 0; i < totalAuths; i++ {
		fileName := fmt.Sprintf("codex-bulk-%02d-plus.json", i)
		authPath := filepath.Join(tempDir, fileName)
		if errWrite := os.WriteFile(authPath, []byte(`{"type":"codex"}`), 0o600); errWrite != nil {
			t.Fatalf("write auth file %s: %v", fileName, errWrite)
		}

		accountID := fmt.Sprintf("acct-bulk-%02d", i)
		accessToken := fmt.Sprintf("token-bulk-%02d", i)
		expectedEmails[fileName] = fmt.Sprintf("fresh-%s@example.com", accountID)

		auth := &coreauth.Auth{
			ID:       fmt.Sprintf("codex-bulk-auth-%02d", i),
			FileName: fileName,
			Provider: "codex",
			Attributes: map[string]string{
				"path": authPath,
			},
			Metadata: map[string]any{
				"access_token": accessToken,
				"account_id":   accountID,
			},
		}
		auth.Metadata = quota.PersistQuotaToMetadata(auth.Metadata, &quota.CodexQuotaCacheEntry{
			QuotaInfo: &quota.CodexQuotaInfo{
				AccountID: accountID,
				Email:     fmt.Sprintf("stale-%02d@example.com", i),
				PlanType:  "plus",
				RateLimit: &quota.RateLimitInfo{
					Allowed: true,
					PrimaryWindow: &quota.LimitWindow{
						UsedPercent: i,
					},
				},
			},
			FetchedAt:   time.Now(),
			ExpiresAt:   time.Now().Add(5 * time.Minute),
			AccountID:   accountID,
			AccessToken: accessToken,
		})

		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %s: %v", fileName, errRegister)
		}
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	var fetchCalls atomic.Int32
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls.Add(1)
		return &quota.CodexQuotaInfo{
			AccountID: accountID,
			Email:     fmt.Sprintf("fresh-%s@example.com", accountID),
			PlanType:  "plus",
			RateLimit: &quota.RateLimitInfo{
				Allowed: true,
				PrimaryWindow: &quota.LimitWindow{
					UsedPercent: 100,
				},
			},
		}, nil
	}
	t.Cleanup(func() {
		fetchCodexQuotaForAuth = originalFetcher
	})

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	ctx.Request.Header.Set("Referer", "http://localhost:8317/management.html#/auth-files")

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := int(fetchCalls.Load()); got != totalAuths {
		t.Fatalf("fetch calls = %d, want %d", got, totalAuths)
	}

	var payload struct {
		Files []struct {
			Name       string `json:"name"`
			AuthIndex  string `json:"auth_index"`
			CodexQuota struct {
				Email     string `json:"email"`
				PlanType  string `json:"plan_type"`
				FetchedAt string `json:"fetched_at"`
				RateLimit struct {
					PrimaryWindow struct {
						UsedPercent int `json:"used_percent"`
					} `json:"primary_window"`
				} `json:"rate_limit"`
			} `json:"codex_quota"`
		} `json:"files"`
	}
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &payload); errUnmarshal != nil {
		t.Fatalf("decode payload: %v", errUnmarshal)
	}
	if len(payload.Files) != totalAuths {
		t.Fatalf("files len = %d, want %d", len(payload.Files), totalAuths)
	}

	for _, file := range payload.Files {
		expectedEmail, ok := expectedEmails[file.Name]
		if !ok {
			continue
		}
		if file.CodexQuota.Email != expectedEmail {
			t.Fatalf("quota email for %s = %q, want %q", file.Name, file.CodexQuota.Email, expectedEmail)
		}
		if file.CodexQuota.PlanType != "plus" {
			t.Fatalf("quota plan_type for %s = %q, want plus", file.Name, file.CodexQuota.PlanType)
		}
		if file.CodexQuota.FetchedAt == "" {
			t.Fatalf("expected fetched_at for %s to be populated", file.Name)
		}
		if file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent != 100 {
			t.Fatalf("used_percent for %s = %d, want 100", file.Name, file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent)
		}
	}
}
