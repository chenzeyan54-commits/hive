package config

import (
	"strings"
	"testing"
)

func standbyBaseConfig() *Config {
	return &Config{
		Project: ProjectConfig{Org: "hivecommons", Repos: []string{"hive"}},
		GitHub:  GitHubConfig{Token: "token"},
		Agents: map[string]AgentConfig{
			"quality": {Backend: "copilot", Model: "claude-sonnet-4-6"},
		},
	}
}

func TestStandbyConfigDefaultsToT1AndPrivateReposOptOut(t *testing.T) {
	cfg := standbyBaseConfig()
	cfg.applyDefaults()
	if got := cfg.Agents["quality"].Standby.MinModelCapability; got != "T1" {
		t.Fatalf("default min_model_capability = %q, want T1", got)
	}
	if cfg.Hub.StandbyAllowPrivateRepos {
		t.Fatal("standby_allow_private_repos defaulted on; want default off")
	}
}

func TestStandbyEnabledRequiresApprovedContributor(t *testing.T) {
	cfg := standbyBaseConfig()
	agent := cfg.Agents["quality"]
	agent.Standby.Enabled = true
	cfg.Agents["quality"] = agent
	cfg.applyDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "hub.standby_contributors") {
		t.Fatalf("Validate error = %v, want approved-list requirement", err)
	}
}

func TestStandbyRejectsUnknownFloor(t *testing.T) {
	cfg := standbyBaseConfig()
	agent := cfg.Agents["quality"]
	agent.Standby.MinModelCapability = "unknown"
	cfg.Agents["quality"] = agent
	cfg.applyDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "standby.min_model_capability") {
		t.Fatalf("Validate error = %v, want min_model_capability rejection", err)
	}
}

func TestStandbyModelTiersRejectDuplicatesAndBadTier(t *testing.T) {
	cfg := standbyBaseConfig()
	cfg.Hub.StandbyModelTiers = []StandbyModelTier{
		{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high", Tier: "T2"},
		{Backend: "claude", Model: "claude-opus-5", ReasoningEffort: "high", Tier: "T3"},
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate configuration") {
		t.Fatalf("Validate duplicate error = %v", err)
	}

	cfg = standbyBaseConfig()
	cfg.Hub.StandbyModelTiers = []StandbyModelTier{{Backend: "claude", Model: "claude-opus-5", Tier: "unknown"}}
	cfg.applyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "standby_model_tiers tier") {
		t.Fatalf("Validate tier error = %v", err)
	}
}

func TestStandbyContributorsAreNormalized(t *testing.T) {
	cfg := standbyBaseConfig()
	cfg.Hub.StandbyContributors = []string{" Alice ", "alice", "Bob"}
	cfg.applyDefaults()
	got := strings.Join(cfg.Hub.StandbyContributors, ",")
	if got != "alice,bob" {
		t.Fatalf("standby contributors = %q, want alice,bob", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate normalized contributors: %v", err)
	}
}

func TestStandbyNegativeDailyCapRejected(t *testing.T) {
	cfg := standbyBaseConfig()
	agent := cfg.Agents["quality"]
	agent.Standby.DailyCapPerContributor = -1
	cfg.Agents["quality"] = agent
	cfg.applyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "daily_cap_per_contributor") {
		t.Fatalf("Validate negative cap error = %v", err)
	}
}
