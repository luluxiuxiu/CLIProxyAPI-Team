package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestAPICallTransportDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "direct"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}
	if httpTransport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestAPICallTransportInvalidAuthFallsBackToGlobalProxy(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
		},
	}

	transport := h.apiCallTransport(&coreauth.Auth{ProxyURL: "bad-value"})
	httpTransport, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", transport)
	}

	req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if errRequest != nil {
		t.Fatalf("http.NewRequest returned error: %v", errRequest)
	}

	proxyURL, errProxy := httpTransport.Proxy(req)
	if errProxy != nil {
		t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
	}
	if proxyURL == nil || proxyURL.String() != "http://global-proxy.example.com:8080" {
		t.Fatalf("proxy URL = %v, want http://global-proxy.example.com:8080", proxyURL)
	}
}

func TestAPICallTransportAPIKeyAuthFallsBackToConfigProxyURL(t *testing.T) {
	t.Parallel()

	h := &Handler{
		cfg: &config.Config{
			SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"},
			GeminiKey: []config.GeminiKey{{
				APIKey:   "gemini-key",
				ProxyURL: "http://gemini-proxy.example.com:8080",
			}},
			ClaudeKey: []config.ClaudeKey{{
				APIKey:   "claude-key",
				ProxyURL: "http://claude-proxy.example.com:8080",
			}},
			CodexKey: []config.CodexKey{{
				APIKey:   "codex-key",
				ProxyURL: "http://codex-proxy.example.com:8080",
			}},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name:    "bohe",
				BaseURL: "https://bohe.example.com",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{
					APIKey:   "compat-key",
					ProxyURL: "http://compat-proxy.example.com:8080",
				}},
			}},
		},
	}

	cases := []struct {
		name      string
		auth      *coreauth.Auth
		wantProxy string
	}{
		{
			name: "gemini",
			auth: &coreauth.Auth{
				Provider:   "gemini",
				Attributes: map[string]string{"api_key": "gemini-key"},
			},
			wantProxy: "http://gemini-proxy.example.com:8080",
		},
		{
			name: "claude",
			auth: &coreauth.Auth{
				Provider:   "claude",
				Attributes: map[string]string{"api_key": "claude-key"},
			},
			wantProxy: "http://claude-proxy.example.com:8080",
		},
		{
			name: "codex",
			auth: &coreauth.Auth{
				Provider:   "codex",
				Attributes: map[string]string{"api_key": "codex-key"},
			},
			wantProxy: "http://codex-proxy.example.com:8080",
		},
		{
			name: "openai-compatibility",
			auth: &coreauth.Auth{
				Provider: "bohe",
				Attributes: map[string]string{
					"api_key":      "compat-key",
					"compat_name":  "bohe",
					"provider_key": "bohe",
				},
			},
			wantProxy: "http://compat-proxy.example.com:8080",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			transport := h.apiCallTransport(tc.auth)
			httpTransport, ok := transport.(*http.Transport)
			if !ok {
				t.Fatalf("transport type = %T, want *http.Transport", transport)
			}

			req, errRequest := http.NewRequest(http.MethodGet, "https://example.com", nil)
			if errRequest != nil {
				t.Fatalf("http.NewRequest returned error: %v", errRequest)
			}

			proxyURL, errProxy := httpTransport.Proxy(req)
			if errProxy != nil {
				t.Fatalf("httpTransport.Proxy returned error: %v", errProxy)
			}
			if proxyURL == nil || proxyURL.String() != tc.wantProxy {
				t.Fatalf("proxy URL = %v, want %s", proxyURL, tc.wantProxy)
			}
		})
	}
}

func TestAuthByIndexDistinguishesSharedAPIKeysAcrossProviders(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	geminiAuth := &coreauth.Auth{
		ID:       "gemini:apikey:123",
		Provider: "gemini",
		Attributes: map[string]string{
			"api_key": "shared-key",
		},
	}
	compatAuth := &coreauth.Auth{
		ID:       "openai-compatibility:bohe:456",
		Provider: "bohe",
		Label:    "bohe",
		Attributes: map[string]string{
			"api_key":      "shared-key",
			"compat_name":  "bohe",
			"provider_key": "bohe",
		},
	}

	if _, errRegister := manager.Register(context.Background(), geminiAuth); errRegister != nil {
		t.Fatalf("register gemini auth: %v", errRegister)
	}
	if _, errRegister := manager.Register(context.Background(), compatAuth); errRegister != nil {
		t.Fatalf("register compat auth: %v", errRegister)
	}

	geminiIndex := geminiAuth.EnsureIndex()
	compatIndex := compatAuth.EnsureIndex()
	if geminiIndex == compatIndex {
		t.Fatalf("shared api key produced duplicate auth_index %q", geminiIndex)
	}

	h := &Handler{authManager: manager}

	gotGemini := h.authByIndex(geminiIndex)
	if gotGemini == nil {
		t.Fatal("expected gemini auth by index")
	}
	if gotGemini.ID != geminiAuth.ID {
		t.Fatalf("authByIndex(gemini) returned %q, want %q", gotGemini.ID, geminiAuth.ID)
	}

	gotCompat := h.authByIndex(compatIndex)
	if gotCompat == nil {
		t.Fatal("expected compat auth by index")
	}
	if gotCompat.ID != compatAuth.ID {
		t.Fatalf("authByIndex(compat) returned %q, want %q", gotCompat.ID, compatAuth.ID)
	}
}

func TestPersistCodexQuotaSnapshotSyncsPlanTypeToAuth(t *testing.T) {
	t.Parallel()

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"plan_type": "free",
		},
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}

	h := &Handler{
		authManager:       manager,
		codexQuotaManager: quota.NewCodexQuotaManager(time.Minute),
	}

	quotaInfo := &quota.CodexQuotaInfo{PlanType: "plus"}
	h.persistCodexQuotaSnapshot(context.Background(), registered, registered.Index, "acct-1", "token-1", quotaInfo)

	updated := h.authByIndex(registered.Index)
	if updated == nil {
		t.Fatal("expected updated auth")
	}
	if got := updated.Metadata["plan_type"]; got != "plus" {
		t.Fatalf("metadata plan_type = %v, want plus", got)
	}
	if got := updated.Attributes["plan_type"]; got != "plus" {
		t.Fatalf("attributes plan_type = %q, want plus", got)
	}
	entry, ok := quota.ReadQuotaFromMetadata(updated.Metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil {
		t.Fatal("expected persisted codex quota cache entry")
	}
	if entry.QuotaInfo.PlanType != "plus" {
		t.Fatalf("quota cache plan_type = %q, want plus", entry.QuotaInfo.PlanType)
	}
}

func TestAPICallCodexQuotaCacheHitSyncsAuthSnapshot(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Metadata: map[string]any{
			"access_token": "token-1",
			"account_id":   "acct-1",
			"plan_type":    "free",
		},
		Attributes: map[string]string{
			"plan_type": "free",
		},
	}
	registered, errRegister := manager.Register(context.Background(), auth)
	if errRegister != nil {
		t.Fatalf("register codex auth: %v", errRegister)
	}

	h := &Handler{
		authManager:       manager,
		codexQuotaManager: quota.NewCodexQuotaManager(time.Minute),
	}
	h.codexQuotaManager.UpdateCache(registered.Index, "acct-1", &quota.CodexQuotaInfo{
		AccountID: "acct-1",
		Email:     "fresh@example.com",
		PlanType:  "plus",
		RateLimit: &quota.RateLimitInfo{
			Allowed:      false,
			LimitReached: true,
			PrimaryWindow: &quota.LimitWindow{
				UsedPercent: 100,
			},
		},
	}, "token-1")

	requestBody, errMarshal := json.Marshal(map[string]any{
		"auth_index": registered.Index,
		"method":     http.MethodGet,
		"url":        quota.CodexUsageEndpoint,
		"header": map[string]string{
			"Authorization": "Bearer $TOKEN$",
		},
	})
	if errMarshal != nil {
		t.Fatalf("marshal request body: %v", errMarshal)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/api-call", strings.NewReader(string(requestBody)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.APICall(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var response apiCallResponse
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &response); errUnmarshal != nil {
		t.Fatalf("decode response: %v", errUnmarshal)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upstream status = %d, want %d", response.StatusCode, http.StatusOK)
	}
	if response.Header["X-Codex-Quota-Cached"][0] != "true" {
		t.Fatalf("expected cache hit header, got %#v", response.Header)
	}

	updated := h.authByIndex(registered.Index)
	if updated == nil {
		t.Fatal("expected updated auth")
	}
	entry, ok := quota.ReadQuotaFromMetadata(updated.Metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil {
		t.Fatal("expected persisted codex quota cache entry")
	}
	if entry.QuotaInfo.Email != "fresh@example.com" {
		t.Fatalf("quota cache email = %q, want fresh@example.com", entry.QuotaInfo.Email)
	}
	if entry.QuotaInfo.PlanType != "plus" {
		t.Fatalf("quota cache plan_type = %q, want plus", entry.QuotaInfo.PlanType)
	}
	if entry.QuotaInfo.RateLimit == nil || entry.QuotaInfo.RateLimit.PrimaryWindow == nil || entry.QuotaInfo.RateLimit.PrimaryWindow.UsedPercent != 100 {
		t.Fatalf("quota cache used_percent = %+v, want 100", entry.QuotaInfo.RateLimit)
	}
	if updated.Metadata["plan_type"] != "plus" {
		t.Fatalf("metadata plan_type = %v, want plus", updated.Metadata["plan_type"])
	}
	if updated.Attributes["plan_type"] != "plus" {
		t.Fatalf("attributes plan_type = %q, want plus", updated.Attributes["plan_type"])
	}

	cached := h.codexQuotaManager.GetQuota(registered.Index)
	if cached == nil || cached.QuotaInfo == nil || cached.QuotaInfo.Email != "fresh@example.com" {
		t.Fatal("expected codex quota manager cache to be refreshed")
	}
}
