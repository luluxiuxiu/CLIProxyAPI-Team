package management

import (
	"context"
	"encoding/json"
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

func TestListAuthFiles_RefreshesCodexQuotaAsynchronously(t *testing.T) {
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
			PlanType:  "free",
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
			PlanType:  "free",
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
	if file.CodexQuota.PlanType != "free" {
		t.Fatalf("quota plan_type = %q, want free", file.CodexQuota.PlanType)
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
