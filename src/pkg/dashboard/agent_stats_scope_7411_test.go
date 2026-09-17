package dashboard

import (
	"encoding/json"
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
// These tests pin the three halves of the fix: the seed is gone, health stats
// are scoped to the CI owner on read (so already-seeded spokes are repaired),
// and an unmeasured percentage renders — instead of 0%.

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

func TestSeedStatsShipHealthStripOnlyToCIOwner(t *testing.T) {
	dir := seedStatsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		agent := e.Name()
		path := filepath.Join(dir, agent, "stats.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var stats []map[string]any
		if err := json.Unmarshal(data, &stats); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		for _, s := range stats {
			src, _ := s["source"].(string)
			key, _ := s["key"].(string)
			if agent == "ci-maintainer" {
				continue
			}
			if src == "health" {
				t.Errorf("%s seeds a health-sourced stat %q — health stats describe the primary repo's workflows and belong to the CI owner only (#7411)", path, key)
			}
			if key == "coverage" {
				t.Errorf("%s seeds a coverage stat — coverage is collected for ci-maintainer only, so this renders a fabricated number (#7411)", path)
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

func statFixture(key, source string) map[string]any {
	return map[string]any{"key": key, "label": key, "source": source, "field": key, "style": "dot"}
}

func TestScopeHealthStatsDropsHealthForNonCIAgent(t *testing.T) {
	stats := []any{
		statFixture("coverage", "agentMetrics"),
		statFixture("brew", "health"),
		statFixture("ci", "health"),
		statFixture("openPrs", "status"),
	}
	got := scopeHealthStats("reviewer", stats, &config.Config{})
	if len(got) != 2 {
		t.Fatalf("expected the 2 non-health stats to survive, got %d: %v", len(got), got)
	}
	for _, raw := range got {
		m := raw.(map[string]any)
		if m["source"] == "health" {
			t.Errorf("health stat %v survived scoping for a non-CI agent (#7411)", m["key"])
		}
	}
}

func TestScopeHealthStatsKeepsHealthForCIOwner(t *testing.T) {
	stats := []any{statFixture("brew", "health"), statFixture("ci", "health")}

	if got := scopeHealthStats("ci-maintainer", stats, &config.Config{}); len(got) != 2 {
		t.Errorf("ci-maintainer lost its own health stats: %v", got)
	}

	// An operator-renamed CI agent is identified by role, and a replica by its
	// base name — both still own the health strip.
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"build-cop":      {Role: "ci-maintainer"},
		"ci-maintainer2": {ReplicaOf: "ci-maintainer"},
		"reviewer":       {Role: "reviewer"},
	}}
	if got := scopeHealthStats("build-cop", stats, cfg); len(got) != 2 {
		t.Errorf("agent with role ci-maintainer lost its health stats: %v", got)
	}
	if got := scopeHealthStats("ci-maintainer2", stats, cfg); len(got) != 2 {
		t.Errorf("ci-maintainer replica lost its health stats: %v", got)
	}
	if got := scopeHealthStats("reviewer", stats, cfg); len(got) != 0 {
		t.Errorf("reviewer kept %d health stats: %v", len(got), got)
	}
}

func TestScopeHealthStatsHandlesNilConfigAndJunk(t *testing.T) {
	if got := scopeHealthStats("reviewer", nil, nil); len(got) != 0 {
		t.Errorf("nil stats should stay empty, got %v", got)
	}
	// A nil config cannot prove another agent owns CI, so only the literal
	// ci-maintainer name does — and non-map entries are passed through.
	if got := scopeHealthStats("ci-maintainer", []any{statFixture("brew", "health")}, nil); len(got) != 1 {
		t.Errorf("ci-maintainer with nil config lost its health stats: %v", got)
	}
	got := scopeHealthStats("reviewer", []any{"not-a-map", statFixture("brew", "health")}, nil)
	if len(got) != 1 || got[0] != "not-a-map" {
		t.Errorf("expected the non-map entry to survive and the health stat to be dropped, got %v", got)
	}
}

// statRenderAssertions executes the dashboard's own renderStatHtml() against a
// measured and an unmeasured coverage stat. It is EXECUTED rather than
// pattern-matched: the bug was a value rule (`v || 0`), and only running it
// proves an absent metric no longer resolves to a confident 0%.
const statRenderAssertions = `
function escapeHtml(s) { return String(s); }
const HEALTH_DESC_DEFAULTS = {};
let FIXTURE = {};
function resolveStatValue(stat) { return FIXTURE[stat.key]; }

function assert(cond, msg) { if (!cond) { console.log('FAIL: ' + msg); process.exitCode = 1; } }

const coverageStat = { key: 'coverage', label: 'Coverage', source: 'agentMetrics', field: 'coverage', style: 'pct-bar', target: 91 };
const ciStat = { key: 'ci', label: 'CI', source: 'health', field: 'ci', style: 'pct' };

FIXTURE = {};
let unmeasured = renderStatHtml(coverageStat, 'reviewer');
assert(!/>0%</.test(unmeasured), 'unmeasured pct-bar still renders 0%: ' + unmeasured);
assert(unmeasured.indexOf('\u2014') !== -1, 'unmeasured pct-bar does not render a dash: ' + unmeasured);
assert(unmeasured.indexOf('not measured') !== -1, 'unmeasured pct-bar does not say it was not measured: ' + unmeasured);
assert(unmeasured.indexOf('width:0%') !== -1, 'unmeasured pct-bar draws a bar: ' + unmeasured);

FIXTURE = { coverage: 88 };
let measured = renderStatHtml(coverageStat, 'ci-maintainer');
assert(measured.indexOf('>88%<') !== -1, 'measured pct-bar lost its value: ' + measured);
assert(measured.indexOf('\u2014') === -1, 'measured pct-bar renders a dash: ' + measured);
assert(measured.indexOf('current') !== -1, 'measured pct-bar lost its current label: ' + measured);

FIXTURE = { coverage: 0 };
let zero = renderStatHtml(coverageStat, 'ci-maintainer');
assert(zero.indexOf('>0%<') !== -1, 'a real 0% measurement must still render 0%: ' + zero);

FIXTURE = {};
let unmeasuredPct = renderStatHtml(ciStat, 'reviewer');
assert(unmeasuredPct.indexOf('0%') === -1, 'unmeasured pct still renders 0%: ' + unmeasuredPct);
assert(unmeasuredPct.indexOf('\u2014') !== -1, 'unmeasured pct does not render a dash: ' + unmeasuredPct);

FIXTURE = { ci: 95 };
let measuredPct = renderStatHtml(ciStat, 'ci-maintainer');
assert(measuredPct.indexOf('>95%<') !== -1, 'measured pct lost its value: ' + measuredPct);
assert(measuredPct.indexOf('good') !== -1, 'measured pct lost its threshold class: ' + measuredPct);
console.log('OK');
`

func TestRenderStatHtmlDoesNotFabricateZeroPercent(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the stat rendering rule was NOT executed by this run")
	}

	script := jsFunc(t, indexHTML(t), "renderStatHtml") + "\n" + statRenderAssertions

	dir := t.TempDir()
	path := filepath.Join(dir, "stat_render.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil || strings.Contains(string(out), "FAIL") {
		t.Fatalf("renderStatHtml assertions failed (#7411):\n%s\nerr=%v", out, err)
	}
}
