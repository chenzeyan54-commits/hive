package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// DefaultTaskListSweepMaxCloses caps the closures a single SweepCompletedTaskListIssues
// tick will perform across all repos, mirroring DefaultAutoMergeSweepMaxMerges. A
// runaway sweep must never spam close-and-comment across the fleet.
const DefaultTaskListSweepMaxCloses = 5

// taskListSweepCommentTemplate is the audit comment posted immediately before
// the close call. It names the sweep and the completed-box count so the action
// is attributable when a maintainer reads the timeline.
const taskListSweepCommentTemplate = "task-list sweep: closing this issue because all %d task-list box(es) in the body are ticked. If this was premature, reopen the issue and uncheck one of the boxes."

const (
	taskListReasonNotHiveFiled  = "not-hive-filed"
	taskListReasonHeld          = "hold-label"
	taskListReasonNoBoxes       = "no-task-list"
	taskListReasonHasUnticked   = "unticked-boxes"
	taskListReasonCommentFailed = "comment-failed"
	taskListReasonCloseFailed   = "close-failed"
	taskListReasonPullRequest   = "pull-request"
)

// TaskListSweepOptions mirrors AutoMergeSweepOptions.
type TaskListSweepOptions struct {
	MaxCloses int
	Audit     func(TaskListSweepEvent)
}

// TaskListSweepEvent describes one close. TotalBoxes is the number of ticked
// checkboxes that made the issue closeable — it is what the audit comment
// names, and the number the dashboard sink surfaces.
type TaskListSweepEvent struct {
	Repo       string
	Number     int
	Author     string
	TotalBoxes int
}

type TaskListSweepResult struct {
	Closed  []TaskListSweepEvent
	Seen    int
	Skipped int
}

// taskListCheckboxRE matches a GitHub-flavored-markdown task-list item at the
// start of a line, allowing arbitrary leading whitespace so nested / indented
// lists count. Capture group 1 is the box contents — a single space means
// unticked, "x" (case-insensitive) means ticked. Bullet markers `-`, `*` and
// `+` are all valid GFM task-list bullets.
var taskListCheckboxRE = regexp.MustCompile(`^[ \t]*[-*+] \[( |[xX])\](?:\s|$)`)

// fenceOpenRE / fenceCloseRE recognise fenced code blocks. A run of THREE OR
// MORE backticks (or tildes) at the start of a line opens a fence; the same
// character with the same or greater count closes it. Boxes inside a fenced
// block are code samples, not task-list items, and must not count — a policy
// template that shows `- [ ]` as an example would otherwise make every
// hive-filed issue that quotes the policy instantly closeable.
var fenceOpenRE = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")

// countTaskListBoxes returns (checked, unchecked) task-list checkboxes in body,
// ignoring any that appear inside a fenced code block.
//
// The parser is deliberately line-oriented and does not attempt the full CommonMark
// grammar: it only needs to recognise ``` / ~~~ fences (the trap the tests guard
// against) and GFM checkbox syntax at the start of a list item. Indented (four-space)
// code blocks are not stripped — the checkbox RE requires a bullet marker and a
// space, which a genuine indented code sample of `    - [ ]` still matches; the
// AttributionTrailerPrefix hive-filed gate already keeps this off human-filed prose.
func countTaskListBoxes(body string) (checked, unchecked int) {
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.Split(body, "\n") {
		if m := fenceOpenRE.FindString(line); m != "" {
			trimmed := strings.TrimLeft(m, " \t")
			ch := trimmed[0]
			ln := len(trimmed)
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = ln
				continue
			}
			// A closing fence must use the same character and be at least as long.
			if ch == fenceChar && ln >= fenceLen {
				inFence = false
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		if inFence {
			continue
		}
		m := taskListCheckboxRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] == " " {
			unchecked++
		} else {
			checked++
		}
	}
	return checked, unchecked
}

// isHiveFiledIssue reports whether issue was filed by the hive itself. It reuses
// the same signal isHumanFiledBugReport uses — every hive-mediated create is
// stamped by AppendTrailer with AttributionTrailerPrefix — so a maintainer's
// issue is never eligible for auto-close no matter what its body looks like.
func isHiveFiledIssue(issue *gh.Issue) bool {
	if issue == nil {
		return false
	}
	return strings.Contains(issue.GetBody(), AttributionTrailerPrefix)
}

// issueLabelNames extracts the *gh.Label slice's names as strings so the shared
// HasHoldLabel helper (which takes []string) can be reused unchanged.
func issueLabelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		out = append(out, l.GetName())
	}
	return out
}

// SweepCompletedTaskListIssues closes every hive-filed open issue whose body
// carries a task list with all boxes ticked. This is the sink that makes the
// finding-granularity policy (Half A) actually drain the backlog: a multi-part
// issue whose body encodes its deliverables as `- [ ]` boxes is closed the tick
// after the last box is ticked.
//
// Safety gates, in the order they run — every one of them is a fail-closed
// keep:
//
//  1. Skip pull requests (Issues.ListByRepo returns both).
//  2. Skip issues whose body does not carry AttributionTrailerPrefix — a
//     maintainer-filed issue is never closed by this sweep, even if it happens
//     to have a completed task list, because the hive did not file the unit of
//     work and cannot decide it is done.
//  3. Skip held issues (HasHoldLabel — same set the rest of the codebase honours).
//  4. Skip issues with zero boxes. An issue whose body is entirely prose has no
//     machine-readable completion criterion; closing it here would break the
//     policy in Half A instead of enforcing it.
//  5. Skip issues with any unticked box.
//  6. Only then post the audit comment and close.
//
// Cap MaxCloses per tick (mirrors DefaultAutoMergeSweepMaxMerges) so a single
// sweep can never mass-close on a bad label rollout.
func (c *Client) SweepCompletedTaskListIssues(ctx context.Context, opts TaskListSweepOptions) (*TaskListSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxCloses := opts.MaxCloses
	if maxCloses <= 0 {
		maxCloses = DefaultTaskListSweepMaxCloses
	}
	result := &TaskListSweepResult{}

	for _, repo := range c.getRepos() {
		if len(result.Closed) >= maxCloses {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenIssuesForTaskListSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Closed) >= maxCloses {
				break
			}
			if issue == nil {
				continue
			}
			result.Seen++
			event, reason, err := c.trySweepTaskListIssue(ctx, repo, owner, repoName, issue)
			if err != nil {
				c.warn("task-list sweep skipped issue", "repo", repo, "issue", issue.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason != "" {
				result.Skipped++
				continue
			}
			result.Closed = append(result.Closed, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			c.info("task-list sweep closed issue", "repo", repo, "issue", event.Number, "author", event.Author, "boxes", event.TotalBoxes)
		}
	}
	return result, nil
}

// listOpenIssuesForTaskListSweep lists every open issue in the repo. Unlike
// listQueuedPullRequestIssues there is no label to key on: a task-list issue is
// recognised by its body, not its labels. Paged so a repo with a long backlog
// still enumerates fully.
func (c *Client) listOpenIssuesForTaskListSweep(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open issues for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// trySweepTaskListIssue evaluates one issue and, if all gates pass, comments and
// closes it. reason is the non-empty skip code when the issue is ineligible;
// err is non-nil only for API failures the caller should log.
func (c *Client) trySweepTaskListIssue(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue) (TaskListSweepEvent, string, error) {
	if issue.IsPullRequest() {
		return TaskListSweepEvent{}, taskListReasonPullRequest, nil
	}
	if !isHiveFiledIssue(issue) {
		return TaskListSweepEvent{}, taskListReasonNotHiveFiled, nil
	}
	if HasHoldLabel(issueLabelNames(issue.Labels)) {
		return TaskListSweepEvent{}, taskListReasonHeld, nil
	}
	checked, unchecked := countTaskListBoxes(issue.GetBody())
	if checked+unchecked == 0 {
		return TaskListSweepEvent{}, taskListReasonNoBoxes, nil
	}
	if unchecked > 0 {
		return TaskListSweepEvent{}, taskListReasonHasUnticked, nil
	}

	number := issue.GetNumber()
	body := fmt.Sprintf(taskListSweepCommentTemplate, checked)
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("posting task-list sweep comment on %s#%d: %w", displayRepo, number, err)
	}
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{State: gh.Ptr("closed")}); err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCloseFailed, fmt.Errorf("closing %s#%d after task-list sweep comment: %w", displayRepo, number, err)
	}

	return TaskListSweepEvent{
		Repo:       displayRepo,
		Number:     number,
		Author:     safeGetLogin(issue.GetUser()),
		TotalBoxes: checked,
	}, "", nil
}
