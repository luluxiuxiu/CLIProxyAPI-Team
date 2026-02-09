package config

// CodexPlanModels defines model availability rules for a plan category.
type CodexPlanModels struct {
	SupportedModels   []string `yaml:"supported-models,omitempty" json:"supported-models,omitempty"`
	UnsupportedModels []string `yaml:"unsupported-models,omitempty" json:"unsupported-models,omitempty"`
}

// CodexAccountModels configures Codex OAuth model policies by plan category.
type CodexAccountModels struct {
	Free CodexPlanModels `yaml:"free,omitempty" json:"free,omitempty"`
	Paid CodexPlanModels `yaml:"paid,omitempty" json:"paid,omitempty"`
}

var defaultCodexFreeSupportedModels = []string{"gpt-5.2", "gpt-5.2-codex"}
var defaultCodexPaidSupportedModels = []string{"gpt-5.3-codex"}

// SanitizeCodexAccountModels normalizes model lists for Codex account policies.
func (cfg *Config) SanitizeCodexAccountModels() {
	if cfg == nil {
		return
	}
	cfg.CodexAccountModels.Free.SupportedModels = NormalizeExcludedModels(cfg.CodexAccountModels.Free.SupportedModels)
	cfg.CodexAccountModels.Free.UnsupportedModels = NormalizeExcludedModels(cfg.CodexAccountModels.Free.UnsupportedModels)
	cfg.CodexAccountModels.Paid.SupportedModels = NormalizeExcludedModels(cfg.CodexAccountModels.Paid.SupportedModels)
	cfg.CodexAccountModels.Paid.UnsupportedModels = NormalizeExcludedModels(cfg.CodexAccountModels.Paid.UnsupportedModels)
}

// EffectiveCodexPlanModels returns the effective model policy for the given plan category.
// If no explicit configuration is provided, it falls back to built-in defaults.
func (cfg *Config) EffectiveCodexPlanModels(planCategory string) CodexPlanModels {
	policy := CodexPlanModels{}
	if cfg != nil {
		switch planCategory {
		case "free":
			policy = cfg.CodexAccountModels.Free
		case "paid":
			policy = cfg.CodexAccountModels.Paid
		default:
			return policy
		}
	} else {
		switch planCategory {
		case "free":
		case "paid":
		default:
			return policy
		}
	}

	if len(policy.SupportedModels) == 0 && len(policy.UnsupportedModels) == 0 {
		switch planCategory {
		case "free":
			policy.SupportedModels = append([]string(nil), defaultCodexFreeSupportedModels...)
		case "paid":
			policy.SupportedModels = append([]string(nil), defaultCodexPaidSupportedModels...)
		}
	}

	policy.SupportedModels = NormalizeExcludedModels(policy.SupportedModels)
	policy.UnsupportedModels = NormalizeExcludedModels(policy.UnsupportedModels)
	return policy
}
