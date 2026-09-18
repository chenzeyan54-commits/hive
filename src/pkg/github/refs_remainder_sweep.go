package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/escalation"
)

// The no-task-list half of the task-list sweep (hivecommons/hive#7641).
//
// #7071's sweep carries a merged PR's outcome back to the issue it references
// — but only when the issue body has `- [ ]` boxes. A hive-filed finding
// written as prose (Danathar/sensi#196: six edits, no boxes) gets nothing:
// the PR that shipped half of it merged with `Refs #196` and a "What this
// deliberately leaves undone" section, and the issue itself stayed silent.
// To the queue it was fresh work again once the 72h merged-claim suppression
// expired; to the maintainer it read as abandoned, when the PR had actually
// handed them a three-line diff.
//
// So, for a hive-filed issue with no task list and at least one merged
// non-closing reference inside the settle window, the sweep posts one
// marker comment quoting each PR's remainder, and — when a `Refs #N` line
// carries RefsNeedsHumanMarker — applies the needs-human label so the issue
// leaves the actionable set (fetchIssues) the way an escalated PR leaves fix
// dispatch. The issue is never closed here: with no boxes there is nothing
// to declare complete.

// remainderHeadingRE picks the PR-body heading that names what a partial PR
// left open. Matched against the heading text only (the `#`s stripped), so a
// heading that merely mentions "remaining" mid-sentence qualifies too — the
// phrasing varies per policy template and per agent.
var remainderHeadingRE = regexp.MustCompile(`(?i)\b(remain\w*|undone|left (?:for|to|open|out)|not (?:in|part of) this|out of scope|follow[- ]?ups?|next (?:steps?|phase))\b`)

// markdownHeadingRE is an ATX heading: 1-6 `#` then a space then text.
var markdownHeadingRE = regexp.MustCompile(`^[ \t]{0,3}(#{1,6})[ \t]+(.*?)[ \t#]*$`)

// remainderQuoteMaxLines bounds how much of a PR body one comment quotes. A
// remainder section is normally a few bullets or a small table; a cap keeps
// a runaway body from turning the issue timeline into a copy of the PR.
const remainderQuoteMaxLines = 60

// refsRemainder is what one merged non-closing PR says it left open.
type refsRemainder struct {
	PR mergedPRRef
	// RefsLine is the PR-body line carrying the `Refs #N` reference, trimmed.
	// The policies ask for the reason to be on that line, so it is the
	// fallback when the body has no remainder section.
	RefsLine string
	// Section is the body of the heading remainderHeadingRE matched, quoted
	// verbatim (bounded by remainderQuoteMaxLines). Empty when there is none.
	Section string
	// NeedsHuman is set when RefsLine carries RefsNeedsHumanMarker.
	NeedsHuman bool
}

// trySweepRefsRemainder is the no-task-list branch of trySweepTaskListIssue.
// The hive-filed and exempt gates have already run. Returns
// taskListReasonNoBoxes when there is nothing to carry back, so the skip
// accounting is unchanged for the common case.
func (c *Client) trySweepRefsRemainder(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, mergedRefs []mergedPRRef) (TaskListSweepEvent, string, error) {
	number := issue.GetNumber()
	var remainders []refsRemainder
	needsHuman := false
	for _, r := range mergedRefs {
		if r.Closing {
			continue
		}
		rem := extractRefsRemainder(r, displayRepo, number)
		needsHuman = needsHuman || rem.NeedsHuman
		remainders = append(remainders, rem)
	}
	if len(remainders) == 0 {
		return TaskListSweepEvent{}, taskListReasonNoBoxes, nil
	}
	sort.Slice(remainders, func(i, j int) bool { return remainders[i].PR.Number < remainders[j].PR.Number })

	desired := renderRefsRemainderComment(remainders, needsHuman)
	existing, err := c.findSweepComment(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("remainder comment on %s#%d: %w", displayRepo, number, err)
	}
	reason := taskListReasonRefsRemainder
	if needsHuman {
		reason = taskListReasonRefsNeedsHuman
	}
	// "Changed" is the only trigger for either write. It makes the label
	// follow the comment's own idempotence: a human who removes needs-human
	// after applying the edits is not re-labelled on the next 15-minute tick
	// (the body is byte-identical, so nothing fires), while a further merged
	// PR changes the body and re-asserts the label. The label goes first so a
	// failed comment write leaves the issue labelled and the next tick, still
	// seeing a changed body, retries both.
	if existing != nil && existing.GetBody() == desired {
		return TaskListSweepEvent{}, reason, nil
	}
	if needsHuman {
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{escalation.NeedsHumanLabel}); err != nil {
			if isGitHubStatus(err, http.StatusNotFound) {
				return TaskListSweepEvent{}, "gone", nil
			}
			return TaskListSweepEvent{}, taskListReasonLabelFailed, fmt.Errorf("labelling %s#%d %s: %w", displayRepo, number, escalation.NeedsHumanLabel, err)
		}
	}
	if err := c.writeSweepComment(ctx, owner, repo, number, existing, desired); err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("remainder comment on %s#%d: %w", displayRepo, number, err)
	}
	c.info("task-list sweep carried a merged Refs PR's remainder back to its issue",
		"repo", displayRepo,
		"issue", number,
		"needs_human", needsHuman,
		"merged_prs", remainderPRNumbers(remainders),
	)
	return TaskListSweepEvent{}, reason, nil
}

func remainderPRNumbers(remainders []refsRemainder) []int {
	out := make([]int, 0, len(remainders))
	for _, r := range remainders {
		out = append(out, r.PR.Number)
	}
	return out
}

// extractRefsRemainder reads what pr says it left open for issue number.
func extractRefsRemainder(pr mergedPRRef, displayRepo string, number int) refsRemainder {
	line := refsLineFor(pr.Body, displayRepo, number)
	return refsRemainder{
		PR:         pr,
		RefsLine:   line,
		Section:    remainderSection(pr.Body),
		NeedsHuman: strings.Contains(strings.ToLower(line), RefsNeedsHumanMarker),
	}
}

// refsLineFor returns the first line of body that references issue number
// WITHOUT a closing keyword — the `Refs #N` line the policies ask agents to
// put the reason on. It reads each line through ParseReferencedIssues, so it
// accepts exactly the spellings collectMergedReferencingPRs accepted.
func refsLineFor(body, displayRepo string, number int) string {
	for _, line := range strings.Split(body, "\n") {
		for _, ref := range ParseReferencedIssues(line, displayRepo) {
			if ref.Issue == number && strings.EqualFold(ref.Repo, displayRepo) {
				return strings.TrimSpace(line)
			}
		}
	}
	return ""
}

// remainderSection returns the text under the first heading in body that
// remainderHeadingRE matches, up to the next heading of the same or a higher
// level, trimmed and bounded by remainderQuoteMaxLines. Headings inside
// fenced code blocks are not headings. Empty when the body has no such
// section.
func remainderSection(body string) string {
	lines := strings.Split(body, "\n")
	inFence := false
	level := 0
	var out []string
	truncated := false
	for _, line := range lines {
		if fenceOpenRE.MatchString(line) {
			inFence = !inFence
			if level > 0 {
				out = append(out, line)
			}
			continue
		}
		if inFence {
			if level > 0 {
				out = append(out, line)
			}
			continue
		}
		if m := markdownHeadingRE.FindStringSubmatch(line); m != nil {
			hl := len(m[1])
			if level > 0 {
				if hl <= level {
					break
				}
				out = append(out, line)
				continue
			}
			if remainderHeadingRE.MatchString(m[2]) {
				level = hl
			}
			continue
		}
		if level > 0 {
			if len(out) >= remainderQuoteMaxLines {
				truncated = true
				break
			}
			out = append(out, line)
		}
	}
	section := strings.TrimSpace(strings.Join(out, "\n"))
	if truncated && section != "" {
		section += "\n…"
	}
	return section
}

// renderRefsRemainderComment builds the marker-prefixed body for the
// no-task-list path. Same stability contract as renderProgressComment: the
// layout is a pure function of the merged set (sorted by PR number), so an
// unchanged tick produces a byte-identical body and no API write.
func renderRefsRemainderComment(remainders []refsRemainder, needsHuman bool) string {
	var b strings.Builder
	fmt.Fprintln(&b, taskListSweepMarker)
	fmt.Fprintf(&b, "task-list sweep: **%d merged PR(s)** reference this issue without closing it, so part of the work is still open here. ", len(remainders))
	fmt.Fprintln(&b, "This issue has no task list, so what remains is quoted from each PR:")
	for _, r := range remainders {
		fmt.Fprintln(&b)
		fmt.Fprintf(&b, "### #%d — %s\n", r.PR.Number, strings.TrimSpace(r.PR.Title))
		if r.PR.URL != "" {
			fmt.Fprintln(&b, r.PR.URL)
		}
		fmt.Fprintln(&b)
		switch {
		case r.Section != "":
			for _, line := range strings.Split(r.Section, "\n") {
				fmt.Fprintf(&b, "> %s\n", line)
			}
		case r.RefsLine != "":
			fmt.Fprintf(&b, "> %s\n", r.RefsLine)
		default:
			fmt.Fprintln(&b, "> _The PR body does not say what remains — see the PR._")
		}
	}
	fmt.Fprintln(&b)
	if needsHuman {
		fmt.Fprintf(&b, "**%s**: a PR above says on its `Refs` line that what remains can only be done by a person. ", escalation.NeedsHumanLabel)
		fmt.Fprintf(&b, "The `%s` label is applied and the hive will not offer this issue as new work while it is on; remove the label or close the issue once the edits are in.\n", escalation.NeedsHumanLabel)
	} else {
		fmt.Fprintln(&b, "The remainder is still open to agents; this issue stays in the queue.")
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the task-list sweep on every cycle; it is not duplicated._")
	return b.String()
}
