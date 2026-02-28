package auth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestManager_ShouldRetryAfterError_RespectsAuthRequestRetryOverride(t *testing.T) {
	m := NewManager(nil, nil, nil)
	m.SetRetryConfig(3, 30*time.Second)

	model := "test-model"
	next := time.Now().Add(5 * time.Second)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"request_retry": float64(0),
		},
		ModelStates: map[string]*ModelState{
			model: {
				Unavailable:    true,
				Status:         StatusError,
				NextRetryAfter: next,
			},
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	_, maxWait := m.retrySettings()
	wait, shouldRetry := m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false for request_retry=0, got true (wait=%v)", wait)
	}

	auth.Metadata["request_retry"] = float64(1)
	if _, errUpdate := m.Update(context.Background(), auth); errUpdate != nil {
		t.Fatalf("update auth: %v", errUpdate)
	}

	wait, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 0, []string{"claude"}, model, maxWait)
	if !shouldRetry {
		t.Fatalf("expected shouldRetry=true for request_retry=1, got false")
	}
	if wait <= 0 {
		t.Fatalf("expected wait > 0, got %v", wait)
	}

	_, shouldRetry = m.shouldRetryAfterError(&Error{HTTPStatus: 500, Message: "boom"}, 1, []string{"claude"}, model, maxWait)
	if shouldRetry {
		t.Fatalf("expected shouldRetry=false on attempt=1 for request_retry=1, got true")
	}
}

func TestManager_MarkResult_RespectsAuthDisableCoolingOverride(t *testing.T) {
	prev := quotaCooldownDisabled.Load()
	quotaCooldownDisabled.Store(false)
	t.Cleanup(func() { quotaCooldownDisabled.Store(prev) })

	m := NewManager(nil, nil, nil)

	auth := &Auth{
		ID:       "auth-1",
		Provider: "claude",
		Metadata: map[string]any{
			"disable_cooling": true,
		},
	}
	if _, errRegister := m.Register(context.Background(), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	model := "test-model"
	m.MarkResult(context.Background(), Result{
		AuthID:   "auth-1",
		Provider: "claude",
		Model:    model,
		Success:  false,
		Error:    &Error{HTTPStatus: 500, Message: "boom"},
	})

	updated, ok := m.GetByID("auth-1")
	if !ok || updated == nil {
		t.Fatalf("expected auth to be present")
	}
	state := updated.ModelStates[model]
	if state == nil {
		t.Fatalf("expected model state to be present")
	}
	if !state.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be zero when disable_cooling=true, got %v", state.NextRetryAfter)
	}
}

func TestIsRequestInvalidError_UsageLimitDoesNotBlockRetry(t *testing.T) {
	t.Parallel()

	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"invalid_request_error","message":"You've hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus), or try again at 2026-02-28T12:00:00Z"}}`,
	}

	if isRequestInvalidError(err) {
		t.Fatalf("usage-limit bad request should not be treated as invalid_request_error")
	}
	if got := statusCodeFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("statusCodeFromError = %d, want %d", got, http.StatusTooManyRequests)
	}
}

func TestApplyAuthFailureState_UsageLimitMapsToQuota(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "auth-usage-limit"}
	resultErr := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"error":{"type":"invalid_request_error","message":"You've hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus), or try again at 2026-02-28T12:00:00Z"}}`,
	}

	applyAuthFailureState(auth, resultErr, nil, now)

	if !auth.Quota.Exceeded {
		t.Fatalf("expected quota exceeded=true")
	}
	if auth.Quota.Reason != "quota" {
		t.Fatalf("quota reason = %q, want %q", auth.Quota.Reason, "quota")
	}
	if auth.StatusMessage != "quota exhausted" {
		t.Fatalf("status message = %q, want %q", auth.StatusMessage, "quota exhausted")
	}
	if auth.NextRetryAfter.IsZero() {
		t.Fatalf("expected NextRetryAfter to be scheduled")
	}
}

func TestStatusCodeFromError_UsageLimitReachedTypeStringMapsTo429(t *testing.T) {
	t.Parallel()

	err := &Error{
		HTTPStatus: http.StatusBadRequest,
		Message:    `{"type":"error","status":429,"error":{"message":"The usage limit has been reached","type":"usage_limit_reached"}}`,
	}

	if got := statusCodeFromError(err); got != http.StatusTooManyRequests {
		t.Fatalf("statusCodeFromError = %d, want %d", got, http.StatusTooManyRequests)
	}
}
