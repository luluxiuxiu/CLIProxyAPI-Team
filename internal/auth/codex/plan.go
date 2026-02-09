package codex

import "strings"

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
