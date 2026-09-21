package planning

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#8011: the dashboard could not say which issues are connected
// to an epic, nor how that epic's plan is progressing. These tests pin the
// reader side of that: the stuck state (a queue nothing is draining is not the
// same as work in flight), and the per-child ownership/PR data the review modal
// needs to show who has each task.

// pendingEpic mints an issue-sourced epic, which lands in the
// plan_status=draft + decompose_pending=true state EpicFromIssue documents.
func pendingEpic(t *testing.T, store *beads.Store, repo string, number int, title string) *beads.Bead {
	t.Helper()
	epic, err := EpicFromIssue(store, github.Issue{Repo: repo, Number: number, Title: title,
		URL: "https://github.com/" + repo + "/issues/1"}, "")
	if err != nil {
		t.Fatalf("EpicFromIssue: %v", err)
	}
	if !DecomposePending(epic) {
		t.Fatalf("freshly minted epic is not decompose_pending: %+v", epic.Metadata)
	}
	return epic
}

func TestDecomposeStuck_OnlyAfterThresholdAndOnlyWhilePending(t *testing.T) {
	store := newStore(t)
	epic := pendingEpic(t, store, "acme/widgets", 42, "Queued epic")
	created := epic.CreatedAt.Time

	if DecomposeStuck(epic, created) {
		t.Error("a just-minted epic must not be stuck — nothing has had a chance to run yet")
	}
	if DecomposeStuck(epic, created.Add(DecomposeStuckAfter-time.Minute)) {
		t.Error("an epic one minute inside the threshold must not be stuck")
	}
	if !DecomposeStuck(epic, created.Add(DecomposeStuckAfter)) {
		t.Error("an epic at exactly the threshold must be stuck")
	}

	// Clearing the pending marker is what decomposition does. However long the
	// epic waited, it is no longer stuck once its plan exists.
	if err := ClearDecomposePending(store, epic.ID); err != nil {
		t.Fatalf("ClearDecomposePending: %v", err)
	}
	built, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if DecomposeStuck(built, created.Add(30*24*time.Hour)) {
		t.Error("a decomposed epic must never be stuck, however old it is")
	}
	if DecomposeStuck(nil, created.Add(30*24*time.Hour)) {
		t.Error("a nil epic must not be reported stuck")
	}
}

func TestListPlansAt_MarksStuckAndSortsItFirst(t *testing.T) {
	store := newStore(t)
	// A draft awaiting review — previously the top of the list.
	draft, _ := decomposeEpic(t, store, "a draft awaiting review", Options{})
	// A queued epic that nothing decomposed.
	stuck := pendingEpic(t, store, "acme/widgets", 42, "z queued forever")

	// At mint time nothing is stuck and the draft still leads.
	fresh := ListPlansAt(map[string]*beads.Store{"architect": store}, stuck.CreatedAt.Time)
	if len(fresh) != 2 {
		t.Fatalf("want 2 plans, got %d", len(fresh))
	}
	if fresh[0].EpicID != draft.ID {
		t.Fatalf("draft should lead before anything is stuck, got %q", fresh[0].EpicTitle)
	}
	for _, p := range fresh {
		if p.Stuck {
			t.Fatalf("nothing should be stuck at mint time: %+v", p)
		}
	}

	// Past the threshold the queued epic is stuck and outranks the draft: the
	// draft is waiting for a decision, the stuck epic is waiting for a rescue.
	later := ListPlansAt(map[string]*beads.Store{"architect": store}, stuck.CreatedAt.Time.Add(DecomposeStuckAfter+time.Hour))
	if len(later) != 2 {
		t.Fatalf("want 2 plans, got %d", len(later))
	}
	if later[0].EpicID != stuck.ID || !later[0].Stuck {
		t.Fatalf("stuck epic should lead and be flagged, got %+v", later[0])
	}
	if later[1].EpicID != draft.ID || later[1].Stuck {
		t.Fatalf("draft should follow and not be flagged, got %+v", later[1])
	}
}

// The pill chip joins on issueRepo#issueNumber, so the listing must carry both
// for an issue-sourced epic — without them the Repositories card cannot tell
// which issue an epic belongs to, which is the whole ask of #8011.
func TestListPlans_CarriesIssueLinkageForTheChipJoin(t *testing.T) {
	store := newStore(t)
	pendingEpic(t, store, "acme/widgets", 42, "Linked epic")

	plans := ListPlans(map[string]*beads.Store{"architect": store})
	if len(plans) != 1 {
		t.Fatalf("want 1 plan, got %d", len(plans))
	}
	if plans[0].IssueRepo != "acme/widgets" || plans[0].IssueNumber != "42" {
		t.Fatalf("issue linkage missing: repo=%q number=%q", plans[0].IssueRepo, plans[0].IssueNumber)
	}
}

func TestGetPlanTree_ChildrenCarryActorAndPR(t *testing.T) {
	store := newStore(t)
	epic, res := decomposeEpic(t, store, "Epic with worked children", Options{})
	if len(res.Children) < 2 {
		t.Fatalf("need >=2 children, got %d", len(res.Children))
	}

	// One child recorded its PR as metadata, the way a working agent does.
	if err := store.SetMetadata(res.Children[0].ID, MetaPRURL, "https://github.com/acme/widgets/pull/7"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	// Another recorded repo + number only (no URL to link).
	if err := store.SetMetadata(res.Children[1].ID, MetaPRRepo, "acme/widgets"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.SetMetadata(res.Children[1].ID, MetaPRNumber, "9"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	tree, err := GetPlanTree(store, epic.ID)
	if err != nil {
		t.Fatalf("GetPlanTree: %v", err)
	}
	byID := map[string]PlanChild{}
	for _, c := range tree.Children {
		byID[c.ID] = c
	}

	withURL := byID[res.Children[0].ID]
	if withURL.PRURL != "https://github.com/acme/widgets/pull/7" || withURL.PRRef != "acme/widgets#7" {
		t.Errorf("pr_url child: ref=%q url=%q", withURL.PRRef, withURL.PRURL)
	}
	refOnly := byID[res.Children[1].ID]
	if refOnly.PRRef != "acme/widgets#9" {
		t.Errorf("pr_repo/pr_number child: ref=%q", refOnly.PRRef)
	}
	if refOnly.PRURL != "" {
		t.Errorf("a repo+number child must NOT get a synthesized URL (the forge host is unknown here), got %q", refOnly.PRURL)
	}
	// Every child carries the actor it was decomposed into, so the review modal
	// can say who has each task.
	for _, c := range tree.Children {
		if c.Actor == "" {
			t.Errorf("child %s has no actor", c.ID)
		}
	}
}

func TestChildPR_ExternalRefAndRejections(t *testing.T) {
	store := newStore(t)

	// external_ref that IS a PR URL (how `bd create --external-ref` records one).
	pr, err := store.Create("worked task", beads.TypeTask, beads.PriorityMedium, "dev",
		"https://ghe.example.com/acme/widgets/pull/12")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref, url := childPR(pr); ref != "acme/widgets#12" || url != "https://ghe.example.com/acme/widgets/pull/12" {
		t.Errorf("GHE pull URL: ref=%q url=%q", ref, url)
	}

	// An ISSUE external_ref is not a PR and must not be reported as one — that
	// is the ref almost every planning bead carries.
	issue, err := store.Create("plain task", beads.TypeTask, beads.PriorityMedium, "dev",
		"https://github.com/acme/widgets/issues/12")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref, url := childPR(issue); ref != "" || url != "" {
		t.Errorf("issue URL must not resolve as a PR: ref=%q url=%q", ref, url)
	}

	// A bare "acme/widgets#3" ref is not a URL and carries no host, so it is not
	// accepted from external_ref either.
	bare, err := store.Create("bare ref task", beads.TypeTask, beads.PriorityMedium, "dev", "acme/widgets#3")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref, url := childPR(bare); ref != "" || url != "" {
		t.Errorf("bare ref must not resolve: ref=%q url=%q", ref, url)
	}
}
