package github

import (
	"context"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
)

// These tests pin the no-task-list half of the sweep (hivecommons/hive#7641):
// a hive-filed prose issue whose merged `Refs #N` PR left part of the work
// open gets ONE comment quoting what remains, and — only when the Refs line
// says `needs-human` — the needs-human label. The shape is
// Danathar/sensi#196 / #198 throughout.

const sensiLikePRBody = `## What changed

` + "`.claude/commands/`" + ` becomes control plane in the two places that do not require editing the permission surface.

Refs #196.

## What this deliberately leaves undone

**The deny rule is the enforcing edit, and it is not in this PR.**

| applied alone | result |
| --- | --- |
| edit 1 — the deny rule | **RED** |

### The three, verbatim

1. ` + "`.claude/settings.json`" + ` — in ` + "`permissions.deny`" + `

## How it was verified

- go test ./...
`

func sweepOnce(t *testing.T, c *Client) *TaskListSweepResult {
	t.Helper()
	result, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result
}

// TestRefsRemainder_NoTaskListIssue_GetsRemainderComment: the core case. A
// hive-filed issue with no boxes and a merged Refs PR gets one marker comment
// naming the PR and quoting its remainder section — and is NOT closed and
// NOT labelled, because the Refs line does not say needs-human.
func TestRefsRemainder_NoTaskListIssue_GetsRemainderComment(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 196, Body: "## Security Finding\n\nSix edits, written as prose.\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 198, Title: "test: own and check .claude/commands/ as control plane", Body: sensiLikePRBody},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	result := sweepOnce(t, c)

	if len(obs.closed) != 0 {
		t.Fatalf("closed = %v; a no-task-list issue must never be closed by the sweep", obs.closed)
	}
	if len(result.Closed) != 0 {
		t.Errorf("result.Closed = %v; want none", result.Closed)
	}
	bodies := obs.commentsPosted[196]
	if len(bodies) != 1 {
		t.Fatalf("commentsPosted[196] = %d; want exactly 1 remainder comment", len(bodies))
	}
	body := bodies[0]
	for _, want := range []string{
		taskListSweepMarker,
		"#198",
		"https://github.com/hivecommons/hive/pull/198",
		"The deny rule is the enforcing edit",
		"| edit 1 — the deny rule | **RED** |",
		"### The three, verbatim", // a deeper heading inside the section is part of it
		"stays in the queue",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("remainder comment lacks %q:\n%s", want, body)
		}
	}
	for _, unwanted := range []string{
		"go test ./...", // the next same-level heading ends the quote
		"## What changed",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("remainder comment quoted past the section (%q):\n%s", unwanted, body)
		}
	}
	if got := obs.labelsAddedTo(196); len(got) != 0 {
		t.Errorf("labels added = %v; a Refs line without needs-human must not label the issue", got)
	}
}

// TestRefsRemainder_NeedsHumanOnRefsLine_LabelsIssueOnce: the explicit form
// on the Refs line applies needs-human, once. The second tick sees a
// byte-identical body and touches neither the comment nor the label — which
// is what lets a human remove the label without the sweep putting it back
// fifteen minutes later.
func TestRefsRemainder_NeedsHumanOnRefsLine_LabelsIssueOnce(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 196, Body: "## Security Finding\n\nprose\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 198, Title: "test: own .claude/commands/", Body: "Ships three of six edits.\n\nRefs #196 — needs-human: the other three edit `.claude/settings.json`, which no agent may touch.\n"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})

	sweepOnce(t, c)
	if got := obs.labelsAddedTo(196); len(got) != 1 || got[0] != escalation.NeedsHumanLabel {
		t.Fatalf("labels added after tick 1 = %v; want exactly [%s]", got, escalation.NeedsHumanLabel)
	}
	bodies := obs.commentsPosted[196]
	if len(bodies) != 1 {
		t.Fatalf("commentsPosted[196] = %d; want 1", len(bodies))
	}
	if !strings.Contains(bodies[0], "**needs-human**") || !strings.Contains(bodies[0], "remove the label or close the issue") {
		t.Errorf("needs-human comment does not tell the human what to do:\n%s", bodies[0])
	}
	if !strings.Contains(bodies[0], "Refs #196 — needs-human:") {
		t.Errorf("comment does not quote the Refs line when the body has no remainder section:\n%s", bodies[0])
	}

	// Tick 2: nothing changed on the forge (the mock never reports labels
	// back, exactly as if a human had removed it).
	sweepOnce(t, c)
	if got := obs.totalCreates(); got != 1 {
		t.Errorf("CreateComment calls after tick 2 = %d; want 1", got)
	}
	if got := obs.totalEdits(); got != 0 {
		t.Errorf("EditComment calls after tick 2 = %d; want 0", got)
	}
	if got := obs.labelsAddedTo(196); len(got) != 1 {
		t.Errorf("labels added after tick 2 = %v; an unchanged body must not re-apply needs-human (a human may have cleared it)", got)
	}
	if len(obs.closed) != 0 {
		t.Errorf("closed = %v; want none", obs.closed)
	}
}

// TestRefsRemainder_NewMergedPRReassertsLabel: a further merged PR changes
// the comment body, so the comment is edited in place (not duplicated) and
// the label is asserted again — new information for the human.
func TestRefsRemainder_NewMergedPRReassertsLabel(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 196, Body: "prose finding\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 198, Title: "first half", Body: "Refs #196 — needs-human: settings.json is out of reach\n"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	c := newTestClient(t, server, org, []string{repo})
	sweepOnce(t, c)
	firstBody := obs.commentsPosted[196][0]
	server.Close()

	merged = append(merged, taskListMergedPR{Number: 199, Title: "second half", Body: "Refs #196 — the doc bullet lands here; the deny rule still needs a human\n"})
	server2, obs2 := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server2.Close()
	obs2.comments[196] = []sweepMockComment{{ID: 1_000_500, Body: firstBody}}
	c2 := newTestClient(t, server2, org, []string{repo})
	sweepOnce(t, c2)

	if obs2.totalCreates() != 0 || obs2.totalEdits() != 1 {
		t.Errorf("tick 2: creates=%d edits=%d; want creates=0 edits=1 (edit in place)", obs2.totalCreates(), obs2.totalEdits())
	}
	edited := obs2.commentsEdited[1_000_500]
	if len(edited) != 1 || !strings.Contains(edited[0], "#198") || !strings.Contains(edited[0], "#199") {
		t.Errorf("edited body does not name both PRs: %v", edited)
	}
	if got := obs2.labelsAddedTo(196); len(got) != 1 {
		t.Errorf("labels added on tick 2 = %v; a changed body must re-assert needs-human", got)
	}
}

// TestRefsRemainder_ProseNeedsHumanIsNotEnough: #7641 chose one explicit form
// over guessing from prose. A PR that says "needs a human" three ways in
// paragraphs but not on its Refs line gets the comment and no label.
func TestRefsRemainder_ProseNeedsHumanIsNotEnough(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 196, Body: "prose finding\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 198, Title: "first half", Body: "Refs #196.\n\n## What remains\n\nA human must apply the deny rule by hand; an agent editing its own permission file is what the rule exists to stop. Only a maintainer can do this.\n"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	sweepOnce(t, c)

	if got := obs.labelsAddedTo(196); len(got) != 0 {
		t.Errorf("labels added = %v; needs-human must come from the Refs line, never from prose", got)
	}
	if got := len(obs.commentsPosted[196]); got != 1 {
		t.Fatalf("commentsPosted[196] = %d; want 1", got)
	}
	if !strings.Contains(obs.commentsPosted[196][0], "A human must apply the deny rule by hand") {
		t.Errorf("comment does not quote the `## What remains` section:\n%s", obs.commentsPosted[196][0])
	}
}

// TestRefsRemainder_ClosingRefDoesNotComment: a merged `Fixes #N` has nothing
// to say about what remains. On a no-task-list issue it is not a remainder,
// so the sweep stays silent exactly as before #7641.
func TestRefsRemainder_ClosingRefDoesNotComment(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 300, Body: "prose finding\n" + hiveTrailer, AuthorLogin: "hive-app[bot]"},
	}
	merged := []taskListMergedPR{
		{Number: 301, Title: "fixed it", Body: "Fixes #300\n\n## What remains\n\nnothing, really\n"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	result := sweepOnce(t, c)

	if got := obs.totalCreates(); got != 0 {
		t.Errorf("CreateComment calls = %d; a closing reference is not a remainder", got)
	}
	if len(obs.closed) != 0 {
		t.Errorf("closed = %v; want none", obs.closed)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d; want 1 (no-task-list)", result.Skipped)
	}
}

// TestRefsRemainder_HumanFiledIssueUntouched: the hive-filed gate runs before
// this path, so a maintainer's own prose issue never gets a sweep comment or
// a label from a merged Refs PR.
func TestRefsRemainder_HumanFiledIssueUntouched(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := []taskListSweepFixture{
		{Number: 400, Body: "a maintainer wrote this, no trailer", AuthorLogin: "some-human", AuthorType: "User"},
	}
	merged := []taskListMergedPR{
		{Number: 401, Title: "partial", Body: "Refs #400 — needs-human: rest is yours\n"},
	}
	server, obs := taskListSweepServer(t, org, repo, fixtures, merged)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	sweepOnce(t, c)

	if obs.totalCreates() != 0 || len(obs.labelsAddedTo(400)) != 0 {
		t.Errorf("human-filed issue touched: creates=%d labels=%v", obs.totalCreates(), obs.labelsAddedTo(400))
	}
}

func TestRemainderSection(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no heading", "Refs #1\n\nplain prose\n", ""},
		{"what remains", "## Why\n\nbecause\n\n## What remains\n\n- a\n- b\n\n## Testing\n\nran it\n", "- a\n- b"},
		{"leaves undone", "### What this deliberately leaves undone\n\nthe deny rule\n", "the deny rule"},
		{"deeper headings stay in", "## Remaining work\n\nintro\n\n### detail\n\nmore\n\n## Next\n\nout\n", "intro\n\n### detail\n\nmore"},
		{"follow-ups", "## Follow-ups\n\n- later\n", "- later"},
		{"heading inside fence is not a heading", "## What remains\n\n```\n## not a heading\n```\nafter\n\n## End\n", "```\n## not a heading\n```\nafter"},
		{"mid-sentence word matches", "## Edits remaining for a human\n\nx\n", "x"},
		{"unrelated heading", "## How it was verified\n\ngo test\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remainderSection(tc.body); got != tc.want {
				t.Errorf("remainderSection(%q) = %q; want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestRemainderSection_Truncates(t *testing.T) {
	var b strings.Builder
	b.WriteString("## What remains\n\n")
	for i := 0; i < remainderQuoteMaxLines*2; i++ {
		b.WriteString("- item\n")
	}
	got := remainderSection(b.String())
	lines := strings.Split(got, "\n")
	if len(lines) > remainderQuoteMaxLines+1 || lines[len(lines)-1] != "…" {
		t.Errorf("truncated section has %d lines, last %q; want at most %d lines ending in …", len(lines), lines[len(lines)-1], remainderQuoteMaxLines+1)
	}
	if strings.Count(got, "- item") >= remainderQuoteMaxLines {
		t.Errorf("quoted %d items; the cap of %d lines did not bound the quote", strings.Count(got, "- item"), remainderQuoteMaxLines)
	}
}

func TestRefsLineFor(t *testing.T) {
	body := "Closes #7\n\nRefs #196 — needs-human: out of reach\nPart of #200\n"
	if got := refsLineFor(body, "o/r", 196); got != "Refs #196 — needs-human: out of reach" {
		t.Errorf("refsLineFor #196 = %q", got)
	}
	if got := refsLineFor(body, "o/r", 200); got != "Part of #200" {
		t.Errorf("refsLineFor #200 = %q", got)
	}
	// A closing keyword is not a reference line.
	if got := refsLineFor(body, "o/r", 7); got != "" {
		t.Errorf("refsLineFor #7 = %q; want empty for a closing reference", got)
	}
	// Another repo's #196 is not this issue.
	if got := refsLineFor("Refs other/repo#196 — needs-human\n", "o/r", 196); got != "" {
		t.Errorf("refsLineFor cross-repo = %q; want empty", got)
	}
}

func TestExtractRefsRemainder_NeedsHumanIsCaseInsensitive(t *testing.T) {
	pr := mergedPRRef{Number: 1, Body: "Refs #5 — NEEDS-HUMAN: apply by hand\n"}
	if !extractRefsRemainder(pr, "o/r", 5).NeedsHuman {
		t.Error("NEEDS-HUMAN on the Refs line was not recognised")
	}
	pr.Body = "Refs #5 — staged; phase two is agent work\n\nneeds-human appears elsewhere in prose\n"
	if extractRefsRemainder(pr, "o/r", 5).NeedsHuman {
		t.Error("needs-human off the Refs line must not count")
	}
}
