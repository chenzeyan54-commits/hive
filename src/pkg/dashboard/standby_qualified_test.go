package dashboard

import (
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func TestQualifiedStandbyCountsPausedLane(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{MinModelCapability: "T2", DailyCapPerContributor: 1}}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "opus", ReasoningEffort: "high", Tier: "T2"}},
		},
	}
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	h.mu.Lock()
	h.connections["c1"] = &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: "alice"},
		cliBackend:      "claude",
		model:           "opus",
		reasoningEffort: "high",
		standby: map[string]StandbyConnectionState{"quality": {
			Lane:            "quality",
			CLIBackend:      "claude",
			Model:           "opus",
			ReasoningEffort: "high",
			DailyCap:        1,
			UpdatedAt:       time.Now(),
		}},
	}
	h.connections["c2"] = &ContributorConnection{
		profile:    &ContributorProfile{GitHubUsername: "mallory"},
		cliBackend: "claude",
		model:      "opus",
		standby:    map[string]StandbyConnectionState{"quality": {Lane: "quality", CLIBackend: "claude", Model: "opus"}},
	}
	h.mu.Unlock()
	counts := h.QualifiedStandbyCounts([]string{"quality"}, nil)
	if counts["quality"] != 1 {
		t.Fatalf("qualified quality count = %d, want 1", counts["quality"])
	}
}

func TestQualifiedStandbyCountsListEmptyMatchesS4(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{MinModelCapability: "T2", DailyCapPerContributor: 1}}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "opus", ReasoningEffort: "high", Tier: "T2"}},
		},
	}
	h := standbyHubWithCandidate(cfg, "alice", "claude", "opus", "high")
	repos := []FrontendRepo{{Name: "repo", Full: "org/repo", ActionableIssues: []any{
		github.Issue{Repo: "org/repo", Number: 1, Title: "Investigate security regression", Labels: []string{"kind/security"}, Lane: "quality"},
	}}}
	counts := h.QualifiedStandbyCounts([]string{"quality"}, repos)
	if counts["quality"] != 1 {
		t.Fatalf("qualified quality count with empty item list = %d, want S4 result 1", counts["quality"])
	}
}

func TestQualifiedStandbyCountsUsesItemTierMatching(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{MinModelCapability: "T3", DailyCapPerContributor: 1}}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice", "bob"},
			StandbyModelTiers: []config.StandbyModelTier{
				{Backend: "claude", Model: "opus", ReasoningEffort: "high", Tier: "T1"},
				{Backend: "claude", Model: "haiku", ReasoningEffort: "low", Tier: "T3"},
			},
			StandbyItemTiers: []config.StandbyItemTier{{Repo: "org/repo", Number: 1, Tier: "T1"}},
		},
	}
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	h.mu.Lock()
	h.connections["alice"] = standbyConnection("alice", "claude", "opus", "high")
	h.connections["bob"] = standbyConnection("bob", "claude", "haiku", "low")
	h.mu.Unlock()
	repos := []FrontendRepo{{Name: "repo", Full: "org/repo", ActionableIssues: []any{
		github.Issue{Repo: "org/repo", Number: 1, Title: "T1 item", Lane: "quality"},
	}}}
	counts := h.QualifiedStandbyCounts([]string{"quality"}, repos)
	if counts["quality"] != 1 {
		t.Fatalf("qualified quality count = %d, want 1; T3 config must not qualify for T1 item", counts["quality"])
	}
}

func TestQualifiedStandbyCountsDoesNotUseT3ProposalWithoutOwnerList(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{"quality": {Standby: &config.StandbyConfig{MinModelCapability: "T3", DailyCapPerContributor: 1}}},
		Hub: config.HubConfig{
			StandbyContributors: []string{"alice"},
			StandbyModelTiers:   []config.StandbyModelTier{{Backend: "claude", Model: "haiku", ReasoningEffort: "low", Tier: "T3"}},
			StandbyItemTiers:    []config.StandbyItemTier{{Repo: "org/repo", Number: 99, Tier: "T3"}},
		},
	}
	h := standbyHubWithCandidate(cfg, "alice", "claude", "haiku", "low")
	repos := []FrontendRepo{{Name: "repo", Full: "org/repo", ActionableIssues: []any{
		github.Issue{Repo: "org/repo", Number: 1, Title: "Fix typo in README", Lane: "quality"},
	}}}
	counts := h.QualifiedStandbyCounts([]string{"quality"}, repos)
	if counts["quality"] != 0 {
		t.Fatalf("qualified quality count = %d, want 0; a T3 proposal must not widen an absent item into the owner list", counts["quality"])
	}
}

func standbyHubWithCandidate(cfg *config.Config, login, backend, model, effort string) *ContributeWSHub {
	s := &Server{deps: &Dependencies{Config: cfg}, logger: slog.Default()}
	h := NewContributeWSHub(slog.Default(), s)
	h.mu.Lock()
	h.connections["c1"] = standbyConnection(login, backend, model, effort)
	h.mu.Unlock()
	return h
}

func standbyConnection(login, backend, model, effort string) *ContributorConnection {
	return &ContributorConnection{
		profile:         &ContributorProfile{GitHubUsername: login},
		cliBackend:      backend,
		model:           model,
		reasoningEffort: effort,
		standby: map[string]StandbyConnectionState{"quality": {
			Lane:            "quality",
			CLIBackend:      backend,
			Model:           model,
			ReasoningEffort: effort,
			DailyCap:        1,
			UpdatedAt:       time.Now(),
		}},
	}
}
