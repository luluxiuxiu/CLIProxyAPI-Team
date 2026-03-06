package auth

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

type codexQuotaSnapshot struct {
	AuthID       string
	Provider     string
	Priority     int
	Allowed      bool
	LimitReached bool
	UsedPercent  int
	ResetAt      time.Time
	BlockedUntil time.Time
	ExpiresAt    time.Time
}

type codexQuotaRanker struct {
	mu          sync.RWMutex
	snapshots   map[string]codexQuotaSnapshot
	buckets     map[string][]string
	nextPruneAt time.Time
}

var globalCodexQuotaRanker = newCodexQuotaRanker()

func newCodexQuotaRanker() *codexQuotaRanker {
	return &codexQuotaRanker{
		snapshots: make(map[string]codexQuotaSnapshot),
		buckets:   make(map[string][]string),
	}
}

func codexQuotaBucketKey(provider string, priority int) string {
	return strings.ToLower(strings.TrimSpace(provider)) + ":" + strconv.Itoa(priority)
}

func buildCodexQuotaSnapshot(auth *Auth, now time.Time) (codexQuotaSnapshot, bool) {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") || len(auth.Metadata) == 0 {
		return codexQuotaSnapshot{}, false
	}
	entry, ok := quota.ReadQuotaFromMetadata(auth.Metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil || entry.QuotaInfo.RateLimit == nil {
		return codexQuotaSnapshot{}, false
	}
	if !entry.ExpiresAt.IsZero() && !entry.ExpiresAt.After(now) {
		return codexQuotaSnapshot{}, false
	}

	rate := entry.QuotaInfo.RateLimit
	usedPercent := -1
	resetAt := time.Time{}
	if rate.PrimaryWindow != nil {
		usedPercent = rate.PrimaryWindow.UsedPercent
		if rate.PrimaryWindow.ResetAt > 0 {
			resetAt = time.Unix(rate.PrimaryWindow.ResetAt, 0)
		}
	}
	if resetAt.IsZero() && rate.SecondaryWindow != nil && rate.SecondaryWindow.ResetAt > 0 {
		resetAt = time.Unix(rate.SecondaryWindow.ResetAt, 0)
	}

	blockedUntil := time.Time{}
	quotaDepleted := !rate.Allowed || rate.LimitReached || usedPercent >= 100
	if quotaDepleted {
		if resetAt.After(now) {
			blockedUntil = resetAt
		} else if rate.SecondaryWindow != nil && rate.SecondaryWindow.ResetAt > 0 {
			secondaryResetAt := time.Unix(rate.SecondaryWindow.ResetAt, 0)
			if secondaryResetAt.After(now) {
				blockedUntil = secondaryResetAt
			}
		}
		if blockedUntil.IsZero() && entry.ExpiresAt.After(now) {
			blockedUntil = entry.ExpiresAt
		}
	}

	return codexQuotaSnapshot{
		AuthID:       strings.TrimSpace(auth.ID),
		Provider:     strings.ToLower(strings.TrimSpace(auth.Provider)),
		Priority:     authPriority(auth),
		Allowed:      rate.Allowed,
		LimitReached: rate.LimitReached,
		UsedPercent:  usedPercent,
		ResetAt:      resetAt,
		BlockedUntil: blockedUntil,
		ExpiresAt:    entry.ExpiresAt,
	}, true
}

func codexQuotaSnapshotBetter(candidate, best codexQuotaSnapshot) bool {
	if candidate.Allowed && !best.Allowed {
		return true
	}
	if !candidate.Allowed && best.Allowed {
		return false
	}
	if !candidate.LimitReached && best.LimitReached {
		return true
	}
	if candidate.LimitReached && !best.LimitReached {
		return false
	}
	if candidate.UsedPercent >= 0 && best.UsedPercent >= 0 {
		if candidate.UsedPercent < best.UsedPercent {
			return true
		}
		if candidate.UsedPercent > best.UsedPercent {
			return false
		}
	}
	if !candidate.ResetAt.IsZero() && !best.ResetAt.IsZero() {
		if candidate.ResetAt.Before(best.ResetAt) {
			return true
		}
		if candidate.ResetAt.After(best.ResetAt) {
			return false
		}
	}
	return candidate.AuthID < best.AuthID
}

func (r *codexQuotaRanker) rebuildBucketLocked(bucketKey string) {
	ids := make([]string, 0)
	for authID, snapshot := range r.snapshots {
		if codexQuotaBucketKey(snapshot.Provider, snapshot.Priority) == bucketKey {
			ids = append(ids, authID)
		}
	}
	if len(ids) == 0 {
		delete(r.buckets, bucketKey)
		return
	}
	sort.SliceStable(ids, func(i, j int) bool {
		left := r.snapshots[ids[i]]
		right := r.snapshots[ids[j]]
		return codexQuotaSnapshotBetter(left, right)
	})
	r.buckets[bucketKey] = ids
}

func (r *codexQuotaRanker) recalculateNextPruneLocked(now time.Time) {
	r.nextPruneAt = time.Time{}
	for _, snapshot := range r.snapshots {
		if snapshot.ExpiresAt.After(now) && (r.nextPruneAt.IsZero() || snapshot.ExpiresAt.Before(r.nextPruneAt)) {
			r.nextPruneAt = snapshot.ExpiresAt
		}
	}
}

func (r *codexQuotaRanker) pruneExpiredLocked(now time.Time) {
	if r.nextPruneAt.IsZero() || r.nextPruneAt.After(now) {
		return
	}
	dirtyBuckets := make(map[string]struct{})
	for authID, snapshot := range r.snapshots {
		if !snapshot.ExpiresAt.IsZero() && !snapshot.ExpiresAt.After(now) {
			dirtyBuckets[codexQuotaBucketKey(snapshot.Provider, snapshot.Priority)] = struct{}{}
			delete(r.snapshots, authID)
		}
	}
	for bucketKey := range dirtyBuckets {
		r.rebuildBucketLocked(bucketKey)
	}
	r.recalculateNextPruneLocked(now)
}

func (r *codexQuotaRanker) Upsert(auth *Auth) {
	if r == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	now := time.Now()
	snapshot, ok := buildCodexQuotaSnapshot(auth, now)
	authID := strings.TrimSpace(auth.ID)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneExpiredLocked(now)

	dirtyBuckets := make(map[string]struct{})
	if existing, exists := r.snapshots[authID]; exists {
		dirtyBuckets[codexQuotaBucketKey(existing.Provider, existing.Priority)] = struct{}{}
	}
	if !ok {
		delete(r.snapshots, authID)
	} else {
		r.snapshots[authID] = snapshot
		dirtyBuckets[codexQuotaBucketKey(snapshot.Provider, snapshot.Priority)] = struct{}{}
	}
	for bucketKey := range dirtyBuckets {
		r.rebuildBucketLocked(bucketKey)
	}
	r.recalculateNextPruneLocked(now)
}

func (r *codexQuotaRanker) Reset(auths []*Auth) {
	if r == nil {
		return
	}
	now := time.Now()
	newSnapshots := make(map[string]codexQuotaSnapshot)
	for i := 0; i < len(auths); i++ {
		snapshot, ok := buildCodexQuotaSnapshot(auths[i], now)
		if !ok || snapshot.AuthID == "" {
			continue
		}
		newSnapshots[snapshot.AuthID] = snapshot
	}

	r.mu.Lock()
	r.snapshots = newSnapshots
	r.buckets = make(map[string][]string)
	for _, snapshot := range r.snapshots {
		bucketKey := codexQuotaBucketKey(snapshot.Provider, snapshot.Priority)
		if _, exists := r.buckets[bucketKey]; exists {
			continue
		}
		r.rebuildBucketLocked(bucketKey)
	}
	r.recalculateNextPruneLocked(now)
	r.mu.Unlock()
}

func (r *codexQuotaRanker) BlockedUntil(auth *Auth, now time.Time) (time.Time, bool) {
	if r == nil || auth == nil {
		return time.Time{}, false
	}
	authID := strings.TrimSpace(auth.ID)
	if authID == "" {
		return time.Time{}, false
	}
	r.mu.Lock()
	r.pruneExpiredLocked(now)
	snapshot, ok := r.snapshots[authID]
	r.mu.Unlock()
	if !ok {
		return time.Time{}, false
	}
	if snapshot.BlockedUntil.After(now) {
		return snapshot.BlockedUntil, true
	}
	return time.Time{}, false
}

func (r *codexQuotaRanker) Reorder(available []*Auth, now time.Time) []*Auth {
	if r == nil || len(available) <= 1 {
		return available
	}
	provider := ""
	priority := 0
	known := false
	availableByID := make(map[string]*Auth, len(available))
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if candidate == nil {
			continue
		}
		availableByID[candidate.ID] = candidate
		if !known {
			provider = candidate.Provider
			priority = authPriority(candidate)
			known = true
		}
	}
	if !known {
		return available
	}

	r.mu.Lock()
	r.pruneExpiredLocked(now)
	orderedIDs := append([]string(nil), r.buckets[codexQuotaBucketKey(provider, priority)]...)
	r.mu.Unlock()
	if len(orderedIDs) == 0 {
		return available
	}

	ordered := make([]*Auth, 0, len(available))
	used := make(map[string]struct{}, len(available))
	for i := 0; i < len(orderedIDs); i++ {
		candidate, ok := availableByID[orderedIDs[i]]
		if !ok || candidate == nil {
			continue
		}
		ordered = append(ordered, candidate)
		used[orderedIDs[i]] = struct{}{}
	}
	for i := 0; i < len(available); i++ {
		candidate := available[i]
		if candidate == nil {
			continue
		}
		if _, ok := used[candidate.ID]; ok {
			continue
		}
		ordered = append(ordered, candidate)
	}
	if len(ordered) == 0 {
		return available
	}
	return ordered
}
