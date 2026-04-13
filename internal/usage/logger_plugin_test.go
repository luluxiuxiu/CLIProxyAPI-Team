package usage

import (
	"context"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
)

func TestRequestStatisticsRecordIncludesLatency(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "test-key",
		Model:       "gpt-5.4",
		RequestedAt: time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC),
		Latency:     1500 * time.Millisecond,
		Detail: coreusage.Detail{
			InputTokens:  10,
			OutputTokens: 20,
			TotalTokens:  30,
		},
	})

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
	if details[0].LatencyMs != 1500 {
		t.Fatalf("latency_ms = %d, want 1500", details[0].LatencyMs)
	}
}

func TestRequestStatisticsMergeSnapshotDedupIgnoresLatency(t *testing.T) {
	stats := NewRequestStatistics()
	timestamp := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	first := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 0,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}
	second := StatisticsSnapshot{
		APIs: map[string]APISnapshot{
			"test-key": {
				Models: map[string]ModelSnapshot{
					"gpt-5.4": {
						Details: []RequestDetail{{
							Timestamp: timestamp,
							LatencyMs: 2500,
							Source:    "user@example.com",
							AuthIndex: "0",
							Tokens: TokenStats{
								InputTokens:  10,
								OutputTokens: 20,
								TotalTokens:  30,
							},
						}},
					},
				},
			},
		},
	}

	result := stats.MergeSnapshot(first)
	if result.Added != 1 || result.Skipped != 0 {
		t.Fatalf("first merge = %+v, want added=1 skipped=0", result)
	}

	result = stats.MergeSnapshot(second)
	if result.Added != 0 || result.Skipped != 1 {
		t.Fatalf("second merge = %+v, want added=0 skipped=1", result)
	}

	snapshot := stats.Snapshot()
	details := snapshot.APIs["test-key"].Models["gpt-5.4"].Details
	if len(details) != 1 {
		t.Fatalf("details len = %d, want 1", len(details))
	}
}

func TestRequestStatisticsSnapshotIncludesCredentialTokenAggregates(t *testing.T) {
	stats := NewRequestStatistics()
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "codex",
		Model:       "gpt-5.3-codex",
		Source:      "codex-user@example.com",
		AuthIndex:   "auth-7",
		RequestedAt: time.Date(2026, 3, 20, 13, 0, 0, 0, time.UTC),
		Detail: coreusage.Detail{
			InputTokens:     100,
			OutputTokens:    40,
			ReasoningTokens: 3,
			CachedTokens:    10,
			TotalTokens:     153,
		},
	})
	stats.Record(context.Background(), coreusage.Record{
		APIKey:      "codex",
		Model:       "gpt-5.3-codex",
		Source:      "codex-user@example.com",
		AuthIndex:   "auth-7",
		RequestedAt: time.Date(2026, 3, 20, 13, 1, 0, 0, time.UTC),
		Failed:      true,
		Detail: coreusage.Detail{
			InputTokens:  15,
			OutputTokens: 5,
			TotalTokens:  20,
		},
	})

	snapshot := stats.Snapshot()
	bySource, ok := snapshot.CredentialsBySource["codex-user@example.com"]
	if !ok {
		t.Fatal("expected credentials_by_source entry")
	}
	if bySource.TotalRequests != 2 || bySource.SuccessCount != 1 || bySource.FailureCount != 1 {
		t.Fatalf("unexpected source aggregate: %+v", bySource)
	}
	if bySource.Tokens.InputTokens != 115 {
		t.Fatalf("source input_tokens = %d, want 115", bySource.Tokens.InputTokens)
	}
	if bySource.Tokens.OutputTokens != 45 {
		t.Fatalf("source output_tokens = %d, want 45", bySource.Tokens.OutputTokens)
	}
	if bySource.Tokens.CachedTokens != 10 {
		t.Fatalf("source cached_tokens = %d, want 10", bySource.Tokens.CachedTokens)
	}
	if bySource.Tokens.TotalTokens != 173 {
		t.Fatalf("source total_tokens = %d, want 173", bySource.Tokens.TotalTokens)
	}

	byAuthIndex, ok := snapshot.CredentialsByAuthIndex["auth-7"]
	if !ok {
		t.Fatal("expected credentials_by_auth_index entry")
	}
	if byAuthIndex.TotalRequests != bySource.TotalRequests || byAuthIndex.Tokens.TotalTokens != bySource.Tokens.TotalTokens {
		t.Fatalf("unexpected auth_index aggregate: %+v", byAuthIndex)
	}
}
