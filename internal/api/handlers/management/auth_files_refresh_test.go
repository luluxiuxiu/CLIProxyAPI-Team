package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestListAuthFiles_RefreshesCodexQuotaBeforeResponding(t *testing.T) {
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
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: tempDir}, manager)
	h.codexQuotaManager = quota.NewCodexQuotaManager(time.Minute)

	originalFetcher := fetchCodexQuotaForAuth
	fetchCalls := 0
	fetchCodexQuotaForAuth = func(ctx context.Context, accessToken, accountID, proxyURL string) (*quota.CodexQuotaInfo, error) {
		fetchCalls++
		if accessToken != "token-1" {
			t.Fatalf("access token = %q, want token-1", accessToken)
		}
		if accountID != "acct-1" {
			t.Fatalf("account id = %q, want acct-1", accountID)
		}
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

	h.ListAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if fetchCalls != 1 {
		t.Fatalf("fetch calls = %d, want 1", fetchCalls)
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
	if file.CodexQuota.Email != "user@example.com" {
		t.Fatalf("quota email = %q, want user@example.com", file.CodexQuota.Email)
	}
	if file.CodexQuota.PlanType != "free" {
		t.Fatalf("quota plan_type = %q, want free", file.CodexQuota.PlanType)
	}
	if file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent != 100 {
		t.Fatalf("used_percent = %d, want 100", file.CodexQuota.RateLimit.PrimaryWindow.UsedPercent)
	}

	updated := h.authByIndex(registered.Index)
	if updated == nil {
		t.Fatal("expected updated auth")
	}
	entry, ok := quota.ReadQuotaFromMetadata(updated.Metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil {
		t.Fatal("expected persisted quota entry in metadata")
	}
	if entry.QuotaInfo.RateLimit == nil || entry.QuotaInfo.RateLimit.PrimaryWindow == nil {
		t.Fatal("expected persisted primary window")
	}
	if entry.QuotaInfo.RateLimit.PrimaryWindow.UsedPercent != 100 {
		t.Fatalf("persisted used_percent = %d, want 100", entry.QuotaInfo.RateLimit.PrimaryWindow.UsedPercent)
	}
}
