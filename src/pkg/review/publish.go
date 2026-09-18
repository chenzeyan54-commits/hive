package review

import (
	"fmt"
	"sort"
	"strings"
)

// The verdict publisher (hivecommons/hive#7469).
//
// The reviewer agent's product is a structured verdict, and it deliberately
// holds NO GitHub write access — reviewer-advisory.md is explicit that the
// asymmetry "a verdict can withhold a merge but never cause one" is what makes
// the agent safe to run. Until now the only consumer of that verdict was
// merge-eligibility (Artifact.HasAggregateApproval -> writeMergeEligible), so
// on a spoke that does not run an auto-merge sweep the verdict terminated in a
// file nobody read: the reviewer's output was unobservable to humans.
//
// This file renders an aggregate verdict as a PR comment body. It performs no
// I/O and knows nothing about GitHub. The SPOKE publishes it (see
// pkg/github.PublishReviewVerdict) using the App token the spoke already
// holds, which is precisely why the reviewer's no-write-access property is
// preserved: nothing here is reachable from the reviewer's own capabilities.

const (
	// VerdictCommentMarker is the invisible HTML-comment tag every published
	// verdict comment carries as its first line, mirroring taskListSweepMarker
	// and levelHoldNoticePrefix. The publisher scans a PR's comments for it so
	// a re-review EDITS the existing comment instead of stacking a new one
	// every cycle.
	//
	// It is deliberately SHA-free: the marker identifies "the hive verdict
	// comment on this PR", of which there must only ever be one. The head SHA
	// lives inside the body (VerdictSHAMarker) so the publisher can tell an
	// already-published verdict from one that needs rewriting.
	VerdictCommentMarker = "<!-- hive:review-verdict -->"

	// verdictSHAMarkerPrefix opens the per-head-SHA marker line. Together with
	// the body being rendered deterministically (no timestamps), this is what
	// makes publishing idempotent on head SHA: re-rendering the same verdict
	// for the same head produces byte-identical output, so the publisher can
	// skip the write entirely.
	verdictSHAMarkerPrefix = "<!-- hive:review-verdict-sha:"

	// maxRenderedFindings bounds how many findings a single comment lists.
	// GitHub caps comment bodies, and a verdict with hundreds of findings is
	// not readable anyway; the remainder is summarised as a count so nothing
	// is silently dropped.
	maxRenderedFindings = 50
)

// VerdictSHAMarker returns the per-head-SHA marker embedded in a published
// verdict comment. Exported so tests and future consumers can assert on the
// exact idempotency token rather than re-deriving its format.
func VerdictSHAMarker(headSHA string) string {
	return verdictSHAMarkerPrefix + strings.TrimSpace(headSHA) + " -->"
}

// SelectPublishable filters an artifact down to the aggregates that are safe
// to publish right now, given the CURRENT head SHA of each open PR.
//
// heads maps "<repo>#<number>" (repo exactly as the aggregate records it,
// i.e. the org-qualified name HasAggregateApproval compares against) to the
// PR's current head SHA. An aggregate is publishable only when:
//
//   - it names a repo and a PR number,
//   - it carries a head SHA (a verdict that cannot prove which commit it
//     judged must never be presented to a human as current), and
//   - that head SHA equals the PR's current head.
//
// The last condition is the whole point: a stale verdict for a superseded
// commit is worse than no comment, because a reader has no way to tell. When
// the artifact holds several aggregates for the same PR (one per re-review),
// the one matching the current head wins; ties keep the last occurrence, which
// is the most recently collected.
func SelectPublishable(artifact Artifact, heads map[string]string) []Aggregate {
	if len(artifact.Items) == 0 || len(heads) == 0 {
		return nil
	}
	chosen := make(map[string]Aggregate, len(artifact.Items))
	for _, item := range artifact.Items {
		repo := strings.TrimSpace(item.Repo)
		sha := strings.TrimSpace(item.HeadSHA)
		if repo == "" || item.Number <= 0 || sha == "" {
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, item.Number)
		if !strings.EqualFold(strings.TrimSpace(heads[key]), sha) {
			continue
		}
		item.Repo, item.HeadSHA = repo, sha
		chosen[key] = item
	}
	if len(chosen) == 0 {
		return nil
	}
	keys := make([]string, 0, len(chosen))
	for k := range chosen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Aggregate, 0, len(keys))
	for _, k := range keys {
		out = append(out, chosen[k])
	}
	return out
}

// RenderVerdictComment renders one aggregate as a GitHub comment body.
//
// The output is DETERMINISTIC — it contains no timestamps and no map-iteration
// order — because the publisher compares the rendered body against the body
// already on the PR to decide whether a write is needed at all. Introducing a
// clock here would turn every cycle into a forge write.
func RenderVerdictComment(a Aggregate) string {
	var b strings.Builder
	b.WriteString(VerdictCommentMarker)
	b.WriteString("\n")
	b.WriteString(VerdictSHAMarker(a.HeadSHA))
	b.WriteString("\n")
	fmt.Fprintf(&b, "## 🐝 Hive review verdict: **%s**\n\n", verdictLabel(a.Verdict))
	if sha := strings.TrimSpace(a.HeadSHA); sha != "" {
		fmt.Fprintf(&b, "Reviewed at head commit `%s`.\n\n", shortSHA(sha))
	}
	b.WriteString("> [!NOTE]\n")
	b.WriteString("> This is **advisory**. The reviewer agent has no GitHub write access and cannot approve, merge, label or close anything; the hive publishes its structured verdict here so a human can read it. A verdict can withhold a merge, never cause one.\n\n")

	b.WriteString(renderPerspectiveTable(a.Perspectives))
	b.WriteString(renderFindings(a.Findings))
	b.WriteString(renderReasons(a.Reasons))

	return b.String()
}

// renderPerspectiveTable renders the per-perspective breakdown, sorted by
// perspective name so the body is stable across runs.
func renderPerspectiveTable(ps map[Perspective]Verdict) string {
	if len(ps) == 0 {
		return "### Perspectives\n\n_No perspective reported a verdict._\n\n"
	}
	names := make([]string, 0, len(ps))
	for p := range ps {
		names = append(names, string(p))
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("### Perspectives\n\n")
	b.WriteString("| Perspective | Verdict |\n| --- | --- |\n")
	for _, n := range names {
		fmt.Fprintf(&b, "| `%s` | %s |\n", n, verdictLabel(ps[Perspective(n)]))
	}
	b.WriteString("\n")
	return b.String()
}

// renderFindings renders each finding with its file:line citation. The
// citation is the evidence that makes the verdict checkable, so a finding that
// carries one must always show it.
func renderFindings(findings []PerspectiveFinding) string {
	if len(findings) == 0 {
		return "### Findings\n\n_No findings were reported._\n\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Findings (%d)\n\n", len(findings))
	shown := findings
	if len(shown) > maxRenderedFindings {
		shown = shown[:maxRenderedFindings]
	}
	for _, pf := range shown {
		f := pf.Finding
		severity := strings.TrimSpace(string(f.Severity))
		if severity == "" {
			severity = "unspecified"
		}
		title := strings.TrimSpace(f.Title)
		if title == "" {
			title = "(untitled finding)"
		}
		fmt.Fprintf(&b, "- **[%s]** `%s` — %s\n", severity, strings.TrimSpace(string(pf.Perspective)), title)
		if cite := Citation(f.File, f.Line); cite != "" {
			fmt.Fprintf(&b, "  - 📍 `%s`\n", cite)
		}
		if summary := strings.TrimSpace(f.Summary); summary != "" {
			fmt.Fprintf(&b, "  - %s\n", collapseLines(summary))
		}
	}
	if len(findings) > len(shown) {
		fmt.Fprintf(&b, "\n_…and %d more finding(s), omitted to keep this comment readable. The full set is in `%s`._\n", len(findings)-len(shown), ReviewVerdictsFile)
	}
	b.WriteString("\n")
	return b.String()
}

func renderReasons(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("### Why this verdict\n\n")
	for _, r := range reasons {
		if r = strings.TrimSpace(r); r != "" {
			fmt.Fprintf(&b, "- %s\n", collapseLines(r))
		}
	}
	b.WriteString("\n")
	return b.String()
}

// Citation renders a finding's location as the "file:line" form the reviewer
// prompt requires of every finding. A finding with no file yields "", and a
// file with no usable line number yields the bare path rather than a
// misleading ":0".
func Citation(file string, line int) string {
	file = strings.TrimSpace(file)
	if file == "" {
		return ""
	}
	if line <= 0 {
		return file
	}
	return fmt.Sprintf("%s:%d", file, line)
}

// verdictLabel renders a verdict for humans. An unrecognised value is passed
// through rather than hidden, so a malformed artifact is visible instead of
// silently rendering as "approve".
func verdictLabel(v Verdict) string {
	switch v {
	case VerdictApprove:
		return "✅ Approve"
	case VerdictChangesRequested:
		return "🔧 Changes requested"
	case VerdictRequiresHuman:
		return "🙋 Requires human review"
	case VerdictReject:
		return "⛔ Reject"
	case "":
		return "❔ No verdict"
	default:
		return string(v)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// collapseLines flattens embedded newlines so a multi-line summary cannot
// break out of its Markdown list item.
func collapseLines(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
