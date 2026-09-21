package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

// hivecommons/hive#8011: once an issue had been planned, the dashboard gave no
// way to see that the issue was connected to an epic, what state the plan was
// in, or how its tasks were progressing — the pill on the Repositories card
// looked identical before and after planning. These tests pin the two halves
// of the fix: the plan LISTING riding the status payload (so the card can join
// on repo#number without one request per pill), and the SPA actually drawing
// the chip, the child ownership, and a planning tile that names what is
// waiting on a human.

func TestBuildPlanningCarriesPlanListingAndStuckCount(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())

	// An issue-sourced epic, queued for the architect and never built — the
	// #8010 outcome the tile used to report as work in flight.
	epic, err := planning.EpicFromIssue(store, github.Issue{
		Repo: "acme/widgets", Number: 42, Title: "Queued epic",
		URL: "https://github.com/acme/widgets/issues/42",
	}, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}

	// Fresh: pending, but not yet stuck.
	fresh := buildPlanningAt(map[string]*beads.Store{"architect": store}, false, 6, epic.CreatedAt.Time)
	if len(fresh.Plans) != 1 {
		t.Fatalf("status payload must carry the plan listing, got %d plans", len(fresh.Plans))
	}
	if fresh.Plans[0].IssueRepo != "acme/widgets" || fresh.Plans[0].IssueNumber != "42" {
		t.Fatalf("listing must carry the issue linkage the pill chip joins on: %+v", fresh.Plans[0])
	}
	if fresh.Stuck != 0 {
		t.Errorf("Stuck: want 0 for a just-minted epic, got %d", fresh.Stuck)
	}
	if fresh.PendingDecompose != 1 {
		t.Errorf("PendingDecompose: want 1, got %d", fresh.PendingDecompose)
	}

	// Past the threshold the same epic is stuck, and the count agrees with the
	// listing — they are derived from the same read, so they cannot disagree.
	later := buildPlanningAt(map[string]*beads.Store{"architect": store}, false, 6,
		epic.CreatedAt.Time.Add(planning.DecomposeStuckAfter+time.Hour))
	if later.Stuck != 1 {
		t.Errorf("Stuck: want 1 past the threshold, got %d", later.Stuck)
	}
	if len(later.Plans) != 1 || !later.Plans[0].Stuck {
		t.Errorf("listing must flag the stuck plan too: %+v", later.Plans)
	}
	if later.PendingDecompose != 1 {
		t.Errorf("a stuck plan is still pending: want PendingDecompose 1, got %d", later.PendingDecompose)
	}

	// The block must survive the JSON round-trip the SPA actually reads.
	raw, err := json.Marshal(later)
	if err != nil {
		t.Fatalf("marshal planning block: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal planning block: %v", err)
	}
	if _, ok := decoded["plans"]; !ok {
		t.Error("planning block has no `plans` key — the card would fall back to no chips at all")
	}
	if got, _ := decoded["stuck"].(float64); got != 1 {
		t.Errorf("planning block `stuck` = %v, want 1", decoded["stuck"])
	}
}

// A hive with no plans must not grow a `plans` key: the chip join reads an
// absent list as "nothing is planned", and an empty array on every status
// payload is pure noise.
func TestBuildPlanningOmitsEmptyPlanListing(t *testing.T) {
	fp := BuildPlanning(map[string]*beads.Store{}, false, 6)
	raw, err := json.Marshal(fp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"plans"`) {
		t.Errorf("empty hive should omit `plans`, got %s", raw)
	}
}

func TestPlanChipWiredIntoRepositoriesCard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Every function the chip path calls must be DEFINED in the document — a
	// referenced-but-undefined name throws at render time.
	for _, def := range []string{
		"function planIssueIndex(",
		"function planChipState(",
		"function planChipHTML(",
		"function planChildStatusColor(",
		"function planNeedsHuman(",
		"function planWaitingSummary(",
		"function planWaitingList(",
		"function planTileTooltip(",
	} {
		if !strings.Contains(html, def) {
			t.Errorf("index.html does not define %q", def)
		}
	}

	// The index must be built from the listing that rides the status payload,
	// not from a per-pill fetch — the explicit ask of #8011 deliverable 1.
	if !strings.Contains(html, "planning) || {}).plans || []") {
		t.Error("planIssueIndex does not read status.planning.plans")
	}
	if !strings.Contains(html, "const planIdx = planIssueIndex();") {
		t.Error("renderRepos does not build the plan index once per repaint")
	}

	// The pill must REPLACE the 📋 button with the chip when the issue has an
	// epic. Both halves matter: the join, and the fallback to the button.
	for _, snippet := range []string{
		"const planned = planIdx.get((repoFull + '#' + i.number).toLowerCase());",
		"const planBtn = planned ? planChipHTML(planned)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}

	// The chip opens THAT epic's plan review — the linkage the card never had.
	if !strings.Contains(html, `data-action="openPlanReview" data-arg0="${esc(p.epicId || '')}"`) {
		t.Error("the plan chip does not open openPlanReview for its epic")
	}

	// The four states the issue names.
	for _, label := range []string{"'⚠ stuck'", "'⧗ planning'", "' task' + (total === 1 ? '' : 's') + ' · review'", "'✓ ' + done + '/' + total"} {
		if !strings.Contains(html, label) {
			t.Errorf("plan chip is missing the %s state", label)
		}
	}
}

func TestPlanReviewShowsWhoHasEachChild(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Deliverable 2: claiming agent, PR link when one exists, status colour.
	for _, snippet := range []string{
		"const who = c.actor ?",
		"const pr = c.prUrl",
		"escapeHtml(c.prRef || 'PR')",
		"planChildStatusColor(c.status)",
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("plan review modal is missing %q", snippet)
		}
	}
}

func TestPlanningTileNamesWhatIsWaitingOnAHuman(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	// Deliverable 3: the tile must not read as a bare "working" count. Both
	// governor tiles consult the same helper and both raise amber on stuck.
	if n := strings.Count(html, "planTileTooltip(plan)"); n < 2 {
		t.Errorf("planTileTooltip is wired into %d planning tile(s), want both", n)
	}
	if !strings.Contains(html, "'Waiting on you:\\n'") {
		t.Error("planTileTooltip does not name what is waiting on a human")
	}
	if !strings.Contains(html, "const alertCls = (awaiting > 0 || stuck > 0) ? ' planning-alert' : '';") {
		t.Error("the governor planning tile does not raise its alert on stuck plans")
	}
	if !strings.Contains(html, "const planClass = (awaiting > 0 || stuck > 0 || paused) ? 'gm-alert' : '';") {
		t.Error("the overview planning metric does not raise its alert on stuck plans")
	}
	// Stuck plans must be excluded from the reassuring "+N queued" suffix.
	if !strings.Contains(html, "const queued = Math.max(0, pending - stuck);") {
		t.Error("stuck plans are still counted as queued — the tile would keep reading as 'working'")
	}
	// The Plans modal must lead with what needs a person.
	if !strings.Contains(html, "planShowModal('Plans', planWaitingSummary(plans) + rows)") {
		t.Error("the Plans modal does not lead with a waiting-on-you summary")
	}
	if !strings.Contains(html, "⚠ stuck — queued 12h+, nothing built") {
		t.Error("the Plans modal row badge never says a plan is stuck")
	}
}
