package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#7411: the `reviewer` agent's card rendered ci-maintainer's
// whole CI/coverage indicator strip, headlined by a fabricated
// "COVERAGE 0% current / goal: 91%" for a metric that is never collected for
// any agent but ci-maintainer.
//
// The reported cause (POST /api/agents cloning another agent's stat set) is not
// what happens: handleAgentCreate never writes stats.json. The strip came from
// a SHIPPED SEED — src/deploy/data/agents/reviewer/stats.json was a verbatim
// copy of ci-maintainer's — which deploy/entrypoint.sh copies into
// /data/agents/ on every container start, so any agent created under that name
// inherited it.
//
// Per the repo owner on the issue, the correct behaviour is that NO stats are
// listed for such an agent: the strip must not render at all, and must not be
// replaced by a placeholder. These tests pin exactly that — nothing for
// reviewer and for a freshly created agent, the real strip for ci-maintainer,
// and no `0%` / `goal: 91%` text anywhere in the non-owner's own rendered
// section.

// ciMaintainerStrip is the stat set the retired reviewer seed copied verbatim:
// the coverage pct-bar plus the repo-wide health indicators.
func ciMaintainerStrip() []any {
	return []any{
		map[string]any{"key": "coverage", "label": "Coverage", "source": "agentMetrics", "field": "coverage", "style": "pct-bar", "target": 91},
		map[string]any{"key": "brew", "label": "Brew", "source": "health", "field": "brew", "style": "dot"},
		map[string]any{"key": "ci", "label": "CI", "source": "health", "field": "ci", "style": "pct"},
		map[string]any{"key": "nightlyRel", "label": "Nightly Rel", "source": "health", "field": "nightlyRel", "style": "dot"},
	}
}

// seedStatsDir is the seed tree the image copies to /opt/hive/seed-data and the
// entrypoint then copies into /data.
func seedStatsDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join("..", "..", "deploy", "data", "agents")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("seed agents dir not found: %v", err)
	}
	return dir
}

func TestSeedStatsShipCIStripOnlyToCIOwner(t *testing.T) {
	dir := seedStatsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "ci-maintainer" {
			continue
		}
		path := filepath.Join(dir, e.Name(), "stats.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var stats []map[string]any
		if err := json.Unmarshal(data, &stats); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, s := range stats {
			if isCIOwnerStat(s) {
				t.Errorf("%s seeds the CI owner's stat %q — the CI/coverage strip belongs to ci-maintainer only, and a seeded copy is inherited by any agent created under that name (#7411)", path, s["key"])
			}
		}
	}
}

func TestSeedStatsHaveNoRetiredReviewerCopy(t *testing.T) {
	path := filepath.Join(seedStatsDir(t), "reviewer", "stats.json")
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s is back — the retired reviewer seed is a verbatim copy of ci-maintainer's CI strip and is inherited by any agent created under that name (#7411)", path)
	}
}

// TestNonCIAgentGetsNoStatsAtAll is assertion 1 on the Go side: an agent that
// does not own CI keeps nothing from a copied ci-maintainer strip — not a
// trimmed strip, not a placeholder entry, nothing.
func TestNonCIAgentGetsNoStatsAtAll(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"reviewer": {Role: "reviewer", Mode: "ADVISORY", OnDemand: true},
	}}
	for _, name := range []string{"reviewer", "brand-new-agent"} {
		got := scopeCIOwnerStats(name, ciMaintainerStrip(), cfg)
		if len(got) != 0 {
			t.Errorf("%s kept %d stat(s) from ci-maintainer's strip, want none: %v", name, len(got), got)
		}
	}
}

// TestFreshAgentDefaultsToNoStats pins the defaulting path a newly created
// agent actually takes: a name the defaults map does not know resolves to no
// stats, before and after scoping.
func TestFreshAgentDefaultsToNoStats(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"brand-new-agent": {}}}
	if got := defaultStatsConfig("brand-new-agent"); len(got) != 0 {
		t.Fatalf("a newly created agent must default to no stats, got %v", got)
	}
	if got := scopeCIOwnerStats("brand-new-agent", defaultStatsConfig("brand-new-agent"), cfg); len(got) != 0 {
		t.Fatalf("a newly created agent must render no stats, got %v", got)
	}
}

// TestNonCIAgentKeepsItsOwnStats guards against over-filtering: scoping removes
// the CI owner's strip, not an agent's own metrics.
func TestNonCIAgentKeepsItsOwnStats(t *testing.T) {
	own := []any{
		map[string]any{"key": "stars", "label": "Stars", "source": "agentMetrics", "field": "stars", "style": "spark"},
		map[string]any{"key": "openPrs", "label": "Open PRs", "source": "status", "field": "openPrCount", "style": "spark"},
	}
	stats := append(ciMaintainerStrip(), own...)
	got := scopeCIOwnerStats("outreach", stats, &config.Config{})
	if len(got) != len(own) {
		t.Fatalf("outreach kept %d stat(s), want its own %d: %v", len(got), len(own), got)
	}
	for _, raw := range got {
		if isCIOwnerStat(raw.(map[string]any)) {
			t.Errorf("a CI-owner stat survived scoping for outreach: %v", raw)
		}
	}
}

func TestScopeCIOwnerStatsKeepsStripForCIOwner(t *testing.T) {
	want := len(ciMaintainerStrip())

	if got := scopeCIOwnerStats("ci-maintainer", ciMaintainerStrip(), &config.Config{}); len(got) != want {
		t.Errorf("ci-maintainer lost its own strip: %v", got)
	}

	// An operator-renamed CI agent is identified by role, and a replica by its
	// base name — both still own the strip.
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"build-cop":      {Role: "ci-maintainer"},
		"ci-maintainer2": {ReplicaOf: "ci-maintainer"},
		"reviewer":       {Role: "reviewer"},
	}}
	if got := scopeCIOwnerStats("build-cop", ciMaintainerStrip(), cfg); len(got) != want {
		t.Errorf("agent with role ci-maintainer lost the strip: %v", got)
	}
	if got := scopeCIOwnerStats("ci-maintainer2", ciMaintainerStrip(), cfg); len(got) != want {
		t.Errorf("ci-maintainer replica lost the strip: %v", got)
	}
	if got := scopeCIOwnerStats("reviewer", ciMaintainerStrip(), cfg); len(got) != 0 {
		t.Errorf("reviewer kept %d stat(s): %v", len(got), got)
	}
}

func TestScopeCIOwnerStatsHandlesNilConfigAndJunk(t *testing.T) {
	if got := scopeCIOwnerStats("reviewer", nil, nil); len(got) != 0 {
		t.Errorf("nil stats should stay empty, got %v", got)
	}
	// A nil config cannot prove another agent owns CI, so only the literal
	// ci-maintainer name does — and non-map entries are passed through.
	if got := scopeCIOwnerStats("ci-maintainer", ciMaintainerStrip(), nil); len(got) != len(ciMaintainerStrip()) {
		t.Errorf("ci-maintainer with nil config lost the strip: %v", got)
	}
	got := scopeCIOwnerStats("reviewer", []any{"not-a-map", map[string]any{"key": "brew", "source": "health"}}, nil)
	if len(got) != 1 || got[0] != "not-a-map" {
		t.Errorf("expected the non-map entry to survive and the health stat to be dropped, got %v", got)
	}
}

// statRenderAssertions executes the dashboard's own renderStatsFromConfig() /
// renderStatHtml() against the stat sets the server-side scoping produces. They
// are EXECUTED rather than pattern-matched: the bug was a value rule, and only
// running the renderer proves reviewer's section is empty and ci-maintainer's
// still carries its measured numbers.
//
// Every assertion is scoped to ONE agent's own rendered section — a page-wide
// search for "0%" or "91" would match unrelated dashboard content and pass for
// the wrong reason.
const statRenderAssertions = `
function escapeHtml(s) { return String(s); }
const HEALTH_DESC_DEFAULTS = {};
let FIXTURE = {};
function resolveStatValue(stat) { return FIXTURE[stat.key]; }

function assert(cond, msg) { if (!cond) { console.log('FAIL: ' + msg); process.exitCode = 1; } }

// 1. reviewer and a freshly created agent render NO strip: no container, no
//    chips, no leftover separators — an empty string.
FIXTURE = {};
const reviewerSection = renderStatsFromConfig(REVIEWER_STATS, 'reviewer');
assert(reviewerSection === '', 'reviewer renders a stat strip: ' + JSON.stringify(reviewerSection));
const freshSection = renderStatsFromConfig(FRESH_STATS, 'brand-new-agent');
assert(freshSection === '', 'a freshly created agent renders a stat strip: ' + JSON.stringify(freshSection));

// 3. the fabricated text is absent from those agents' OWN sections.
[['reviewer', reviewerSection], ['brand-new-agent', freshSection]].forEach(function (pair) {
  const who = pair[0], section = pair[1];
  assert(section.indexOf('0%') === -1, who + ' section still shows a 0% value: ' + section);
  assert(section.indexOf('91') === -1, who + ' section still shows the 91% coverage goal: ' + section);
  assert(section.toUpperCase().indexOf('COVERAGE') === -1, who + ' section still shows a COVERAGE strip: ' + section);
  assert(section.indexOf('ind-dot-item') === -1, who + ' section still renders stat chips: ' + section);
  assert(section.indexOf('agent-indicators') === -1, who + ' section still renders an empty strip container: ' + section);
});

// 2. ci-maintainer still renders its strip, with its real measured values.
FIXTURE = { coverage: 88, brew: 1, ci: 95, nightlyRel: 1 };
const ciSection = renderStatsFromConfig(CI_STATS, 'ci-maintainer');
assert(ciSection.indexOf('COVERAGE') !== -1, 'ci-maintainer lost its coverage strip: ' + ciSection);
assert(ciSection.indexOf('>88%<') !== -1, 'ci-maintainer lost its measured coverage value: ' + ciSection);
assert(ciSection.indexOf('goal: 91%') !== -1, 'ci-maintainer lost its coverage goal: ' + ciSection);
assert(ciSection.indexOf('>95%<') !== -1, 'ci-maintainer lost its measured CI pass rate: ' + ciSection);
assert(ciSection.indexOf('health-dot ok') !== -1, 'ci-maintainer lost its health dots: ' + ciSection);
assert(ciSection.indexOf('\u2014') === -1, 'ci-maintainer renders a dash for a measured value: ' + ciSection);

// A measured 0 is still a measurement and must still render 0%.
FIXTURE = { coverage: 0 };
const zeroSection = renderStatsFromConfig([CI_STATS[0]], 'ci-maintainer');
assert(zeroSection.indexOf('>0%<') !== -1, 'a real 0% measurement must still render 0%: ' + zeroSection);

// An unmeasured metric on the agent that DOES own it renders a dash, never 0%.
FIXTURE = {};
const unmeasured = renderStatsFromConfig([CI_STATS[0]], 'ci-maintainer');
assert(unmeasured.indexOf('>0%<') === -1, 'unmeasured coverage still renders 0%: ' + unmeasured);
assert(unmeasured.indexOf('\u2014') !== -1, 'unmeasured coverage does not render a dash: ' + unmeasured);
console.log('OK');
`

func TestRenderedStripIsAbsentForNonCIAgents(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the stat rendering rules were NOT executed by this run; the Go scoping tests above still ran")
	}

	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"reviewer":        {Role: "reviewer", Mode: "ADVISORY", OnDemand: true},
		"brand-new-agent": {},
		"ci-maintainer":   {},
	}}
	// Feed the renderer exactly what the server-side scoping produces, so this
	// exercises the real pipeline rather than a hand-written expectation.
	fixtures := map[string][]any{
		"REVIEWER_STATS": scopeCIOwnerStats("reviewer", ciMaintainerStrip(), cfg),
		"FRESH_STATS":    scopeCIOwnerStats("brand-new-agent", ciMaintainerStrip(), cfg),
		"CI_STATS":       scopeCIOwnerStats("ci-maintainer", ciMaintainerStrip(), cfg),
	}
	var decls strings.Builder
	for _, name := range []string{"REVIEWER_STATS", "FRESH_STATS", "CI_STATS"} {
		data, err := json.Marshal(fixtures[name])
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&decls, "const %s = %s;\n", name, data)
	}

	html := indexHTML(t)
	script := jsFunc(t, html, "renderStatHtml") + "\n" +
		jsFunc(t, html, "renderStatsFromConfig") + "\n" +
		decls.String() + statRenderAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "stat_render.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil || strings.Contains(string(out), "FAIL") {
		t.Fatalf("stat strip rendering assertions failed (#7411):\n%s\nerr=%v", out, err)
	}
}
