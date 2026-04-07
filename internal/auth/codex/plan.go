package codex

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

// PlanTypeFromIDToken extracts the authoritative plan type from a Codex ID token.
func PlanTypeFromIDToken(idToken string) string {
	trimmed := strings.TrimSpace(idToken)
	if trimmed == "" {
		return ""
	}
	claims, err := ParseJWTToken(trimmed)
	if err != nil || claims == nil {
		return ""
	}
	return strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
}

func planTypeFromQuotaMetadata(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	entry, ok := quota.ReadQuotaFromMetadata(metadata)
	if !ok || entry == nil || entry.QuotaInfo == nil {
		return ""
	}
	return strings.TrimSpace(entry.QuotaInfo.PlanType)
}

// ResolvePlanType prefers quota-derived plan information, then JWT claims, then mutable fields.
func ResolvePlanType(attributes map[string]string, metadata map[string]any) string {
	if metadata != nil {
		if planType := planTypeFromQuotaMetadata(metadata); planType != "" {
			return planType
		}
		if raw, ok := metadata["id_token"].(string); ok {
			if planType := PlanTypeFromIDToken(raw); planType != "" {
				return planType
			}
		}
		if raw, ok := metadata["plan_type"].(string); ok {
			if planType := strings.TrimSpace(raw); planType != "" {
				return planType
			}
		}
	}
	if attributes != nil {
		if planType := PlanTypeFromIDToken(attributes["id_token"]); planType != "" {
			return planType
		}
		if planType := strings.TrimSpace(attributes["plan_type"]); planType != "" {
			return planType
		}
	}
	return ""
}

// SyncPlanType writes the latest resolved plan type back to mutable auth fields.
func SyncPlanType(attributes map[string]string, metadata map[string]any, planType string) (map[string]string, map[string]any) {
	trimmed := strings.TrimSpace(planType)
	if trimmed == "" {
		return attributes, metadata
	}
	if metadata == nil {
		metadata = make(map[string]any, 1)
	}
	metadata["plan_type"] = trimmed
	if attributes == nil {
		attributes = make(map[string]string, 1)
	}
	attributes["plan_type"] = trimmed
	return attributes, metadata
}

// NormalizePlanType canonicalizes plan type strings for comparisons.
func NormalizePlanType(planType string) string {
	trimmed := strings.TrimSpace(planType)
	if trimmed == "" {
		return ""
	}
	trimmed = strings.ToLower(trimmed)
	var b strings.Builder
	b.Grow(len(trimmed))
	lastDash := false
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteRune('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

// PlanCategory maps plan types to a broad category: free, paid, or unknown.
func PlanCategory(planType string) string {
	normalized := NormalizePlanType(planType)
	if normalized == "" {
		return "unknown"
	}
	switch normalized {
	case "free", "free-trial", "trial", "free-tier", "personal-free":
		return "free"
	default:
		return "paid"
	}
}
