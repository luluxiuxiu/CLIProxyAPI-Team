package executor

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseCodexRetryAfter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("resets_in_seconds", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":123}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 123*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 123*time.Second)
		}
	})

	t.Run("prefers resets_at", func(t *testing.T) {
		resetAt := now.Add(5 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":1}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 5*time.Minute {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 5*time.Minute)
		}
	})

	t.Run("fallback when resets_at is past", func(t *testing.T) {
		resetAt := now.Add(-1 * time.Minute).Unix()
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `,"resets_in_seconds":77}}`)
		retryAfter := parseCodexRetryAfter(http.StatusTooManyRequests, body, now)
		if retryAfter == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *retryAfter != 77*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *retryAfter, 77*time.Second)
		}
	})

	t.Run("non-429 status code", func(t *testing.T) {
		body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusBadRequest, body, now); got != nil {
			t.Fatalf("expected nil for non-429, got %v", *got)
		}
	})

	t.Run("non usage_limit_reached error type", func(t *testing.T) {
		body := []byte(`{"error":{"type":"server_error","resets_in_seconds":30}}`)
		if got := parseCodexRetryAfter(http.StatusTooManyRequests, body, now); got != nil {
			t.Fatalf("expected nil for non-usage_limit_reached, got %v", *got)
		}
	})
}

func TestNewCodexStatusErr_NormalizesForbiddenHTMLToServiceUnavailable(t *testing.T) {
	t.Parallel()

	err := newCodexStatusErr(http.StatusForbidden, []byte("<!DOCTYPE html><html><body>Access denied</body></html>"))
	if got := err.StatusCode(); got != http.StatusServiceUnavailable {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusServiceUnavailable)
	}
}

func TestNewCodexStatusErr_PreservesStructuredForbiddenResponses(t *testing.T) {
	t.Parallel()

	err := newCodexStatusErr(http.StatusForbidden, []byte(`{"error":{"message":"forbidden","type":"access_denied"}}`))
	if got := err.StatusCode(); got != http.StatusForbidden {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusForbidden)
	}
}

func TestNewCodexStatusErrTreatsCapacityAsRetryableRateLimit(t *testing.T) {
	body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)

	err := newCodexStatusErr(http.StatusBadRequest, body)

	if got := err.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if err.RetryAfter() != nil {
		t.Fatalf("expected nil explicit retryAfter for capacity fallback, got %v", *err.RetryAfter())
	}
}

func TestParseCodexStreamStatusErr(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("usage_limit_with_resets_in_seconds", func(t *testing.T) {
		event := []byte(`{"type":"error","error":{"type":"usage_limit_reached","resets_in_seconds":120}}`)
		err := parseCodexStreamStatusErr(event, now)
		if err == nil {
			t.Fatalf("expected status error, got nil")
		}
		if got := err.StatusCode(); got != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
		}
		if err.RetryAfter() == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *err.RetryAfter() != 120*time.Second {
			t.Fatalf("retryAfter = %v, want %v", *err.RetryAfter(), 120*time.Second)
		}
	})

	t.Run("usage_limit_with_resets_at", func(t *testing.T) {
		resetAt := now.Add(3 * time.Minute).Unix()
		event := []byte(`{"type":"error","status":429,"error":{"type":"usage_limit_reached","resets_at":` + itoa(resetAt) + `}}`)
		err := parseCodexStreamStatusErr(event, now)
		if err == nil {
			t.Fatalf("expected status error, got nil")
		}
		if got := err.StatusCode(); got != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want %d", got, http.StatusTooManyRequests)
		}
		if err.RetryAfter() == nil {
			t.Fatalf("expected retryAfter, got nil")
		}
		if *err.RetryAfter() != 3*time.Minute {
			t.Fatalf("retryAfter = %v, want %v", *err.RetryAfter(), 3*time.Minute)
		}
	})

	t.Run("non_error_event", func(t *testing.T) {
		event := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
		if got := parseCodexStreamStatusErr(event, now); got != nil {
			t.Fatalf("expected nil, got %+v", *got)
		}
	})
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
