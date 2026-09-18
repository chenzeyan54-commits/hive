package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// sampleAggregate is a verdict shaped like a real one: two perspectives, two
// findings, one of which carries a file:line citation and one of which carries
// only a file.
func sampleAggregate() Aggregate {
	return Aggregate{
		Repo:    "projectbluefin/chairlift",
		Number:  131,
		HeadSHA: "abc123def456789",
		Verdict: VerdictRequiresHuman,
		Reasons: []string{"correctness finding \"refresh gate is not generation-guarded\" is high (threshold high)"},
		Perspectives: map[Perspective]Verdict{
			PerspectiveCorrectness: VerdictRequiresHuman,
			PerspectiveSecurity:    VerdictApprove,
		},
		Findings: []PerspectiveFinding{
			{
				Perspective: PerspectiveCorrectness,
				Finding: outputschema.Finding{
					Title:    "refresh gate is not generation-guarded",
					Severity: outputschema.SeverityHigh,
					Summary:  "A stale refresh can\noverwrite a newer one.",
					File:     "src/flatpak/refresh.go",
					Line:     212,
				},
			},
			{
				Perspective: PerspectiveSecurity,
				Finding: outputschema.Finding{
					Title:    "token is logged at debug",
					Severity: outputschema.SeverityLow,
					Summary:  "Debug logging includes the bearer token.",
					File:     "src/auth/token.go",
				},
			},
		},
	}
}

// TestRenderVerdictComment_ContainsFindingsAndCitations is requirement (c) of
// #7469: the published comment must actually carry the findings and their
// file:line citations, not merely the verdict word.
func TestRenderVerdictComment_ContainsFindingsAndCitations(t *testing.T) {
	body := RenderVerdictComment(sampleAggregate())

	for _, want := range []string{
		VerdictCommentMarker,
		VerdictSHAMarker("abc123def456789"),
		"refresh gate is not generation-guarded",
		"src/flatpak/refresh.go:212",
		"token is logged at debug",
		"correctness",
		"security",
		"high",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered verdict comment is missing %q\n---\n%s", want, body)
		}
	}
	// A finding with no line number must cite the bare path, never ":0".
	if !strings.Contains(body, "`src/auth/token.go`") {
		t.Errorf("finding without a line number lost its file citation\n---\n%s", body)
	}
	if strings.Contains(body, "src/auth/token.go:0") {
		t.Errorf("finding without a line number rendered a bogus :0 citation\n---\n%s", body)
	}
	// Both perspectives must appear in the breakdown with their own verdicts.
	if !strings.Contains(body, "| `correctness` |") || !strings.Contains(body, "| `security` |") {
		t.Errorf("perspective breakdown is missing a perspective row\n---\n%s", body)
	}
	// The multi-line summary must be collapsed so it cannot break the list.
	if !strings.Contains(body, "A stale refresh can overwrite a newer one.") {
		t.Errorf("multi-line finding summary was not collapsed onto one line\n---\n%s", body)
	}
	// The advisory framing is load-bearing: a reader must not mistake this for
	// an approval the reviewer is capable of granting.
	if !strings.Contains(body, "no GitHub write access") {
		t.Errorf("comment does not state that the reviewer holds no write access\n---\n%s", body)
	}
}

// TestRenderVerdictComment_IsDeterministic guards the property the whole
// idempotency design rests on: two renders of the same aggregate must be
// byte-identical, or every publish cycle would rewrite the comment. Map
// iteration order over Perspectives is the specific hazard.
func TestRenderVerdictComment_IsDeterministic(t *testing.T) {
	agg := sampleAggregate()
	agg.Perspectives[PerspectiveStyle] = VerdictApprove
	agg.Perspectives[PerspectiveDocsCurrency] = VerdictApprove
	agg.Perspectives[PerspectiveIntentAlignment] = VerdictApprove
	first := RenderVerdictComment(agg)
	for i := 0; i < 20; i++ {
		if got := RenderVerdictComment(agg); got != first {
			t.Fatalf("render is not deterministic on iteration %d\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}
}

// TestRenderVerdictComment_HeadSHAChangesBody proves a re-review at a new head
// produces a DIFFERENT body, which is what causes the publisher to edit rather
// than no-op.
func TestRenderVerdictComment_HeadSHAChangesBody(t *testing.T) {
	a := sampleAggregate()
	b := sampleAggregate()
	b.HeadSHA = "999888777666555"
	if RenderVerdictComment(a) == RenderVerdictComment(b) {
		t.Fatal("verdicts at different head SHAs rendered identical bodies; the publisher would never update the comment")
	}
}

func TestRenderVerdictComment_NoFindings(t *testing.T) {
	agg := Aggregate{
		Repo:         "org/repo",
		Number:       7,
		HeadSHA:      "deadbeef",
		Verdict:      VerdictApprove,
		Perspectives: map[Perspective]Verdict{PerspectiveCorrectness: VerdictApprove},
	}
	body := RenderVerdictComment(agg)
	if !strings.Contains(body, "No findings were reported") {
		t.Errorf("empty finding set did not render its explicit empty state\n---\n%s", body)
	}
}

func TestRenderVerdictComment_TruncatesFindingList(t *testing.T) {
	agg := sampleAggregate()
	agg.Findings = nil
	for i := 0; i < maxRenderedFindings+7; i++ {
		agg.Findings = append(agg.Findings, PerspectiveFinding{
			Perspective: PerspectiveCorrectness,
			Finding:     outputschema.Finding{Title: "f", Severity: outputschema.SeverityLow, File: "a.go", Line: i + 1},
		})
	}
	body := RenderVerdictComment(agg)
	if !strings.Contains(body, "and 7 more finding(s)") {
		t.Errorf("over-long finding list did not report the omitted remainder\n---\n%s", body)
	}
}

func TestCitation(t *testing.T) {
	cases := []struct {
		file string
		line int
		want string
	}{
		{"a/b.go", 42, "a/b.go:42"},
		{"a/b.go", 0, "a/b.go"},
		{"a/b.go", -3, "a/b.go"},
		{"", 42, ""},
		{"   ", 42, ""},
	}
	for _, tc := range cases {
		if got := Citation(tc.file, tc.line); got != tc.want {
			t.Errorf("Citation(%q, %d) = %q, want %q", tc.file, tc.line, got, tc.want)
		}
	}
}

// TestSelectPublishable_OnlyCurrentHead is the staleness gate: a verdict for a
// superseded commit must never be published as if it were current.
func TestSelectPublishable_OnlyCurrentHead(t *testing.T) {
	art := Artifact{Items: []Aggregate{
		{Repo: "org/a", Number: 1, HeadSHA: "current"},
		{Repo: "org/b", Number: 2, HeadSHA: "stale"},
		{Repo: "org/c", Number: 3, HeadSHA: ""},
		{Repo: "", Number: 4, HeadSHA: "x"},
		{Repo: "org/e", Number: 0, HeadSHA: "x"},
		{Repo: "org/f", Number: 6, HeadSHA: "nosuchpr"},
	}}
	heads := map[string]string{
		"org/a#1": "current",
		"org/b#2": "newer",
		"org/c#3": "whatever",
		"org/e#0": "x",
	}
	got := SelectPublishable(art, heads)
	if len(got) != 1 {
		t.Fatalf("SelectPublishable returned %d items, want 1: %+v", len(got), got)
	}
	if got[0].Repo != "org/a" || got[0].Number != 1 {
		t.Errorf("selected the wrong aggregate: %+v", got[0])
	}
}

// TestSelectPublishable_PrefersMatchingHeadAmongReReviews covers the ordinary
// re-review case: the artifact accumulates one aggregate per head, and only
// the one matching the PR's current head may be published.
func TestSelectPublishable_PrefersMatchingHeadAmongReReviews(t *testing.T) {
	art := Artifact{Items: []Aggregate{
		{Repo: "org/a", Number: 1, HeadSHA: "old", Verdict: VerdictReject},
		{Repo: "org/a", Number: 1, HeadSHA: "new", Verdict: VerdictApprove},
	}}
	got := SelectPublishable(art, map[string]string{"org/a#1": "new"})
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1", len(got))
	}
	if got[0].Verdict != VerdictApprove {
		t.Errorf("published the superseded verdict %q instead of the current one", got[0].Verdict)
	}
}

func TestSelectPublishable_EmptyInputs(t *testing.T) {
	if got := SelectPublishable(Artifact{}, map[string]string{"org/a#1": "x"}); got != nil {
		t.Errorf("empty artifact should select nothing, got %+v", got)
	}
	if got := SelectPublishable(Artifact{Items: []Aggregate{{Repo: "org/a", Number: 1, HeadSHA: "x"}}}, nil); got != nil {
		t.Errorf("no open PRs should select nothing, got %+v", got)
	}
}
