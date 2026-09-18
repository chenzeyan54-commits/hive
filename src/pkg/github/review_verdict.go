package github

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/review"
)

// Verdict publishing (hivecommons/hive#7469).
//
// The reviewer agent produces a structured verdict and holds NO GitHub write
// access — deliberately, because "a verdict can withhold a merge but never
// cause one" is the property its whole design rests on. The SPOKE, however,
// already holds the GitHub App token. So the write happens HERE, in hive's own
// forge client, entirely outside anything the reviewer can reach: the reviewer
// gains no new capability, and the verdict becomes visible to the humans whose
// queue it was supposed to help.
//
// Off by default. cmd/hive only calls this when review.publish_verdicts is
// explicitly enabled on the spoke.

// ReviewVerdictOutcome describes what PublishReviewVerdict did, so callers can
// log and audit a create distinctly from an idempotent no-op.
type ReviewVerdictOutcome string

const (
	// ReviewVerdictCreated means no prior verdict comment existed and one was
	// posted.
	ReviewVerdictCreated ReviewVerdictOutcome = "created"
	// ReviewVerdictUpdated means a prior verdict comment existed with a
	// different body (typically a new head SHA) and was edited IN PLACE.
	ReviewVerdictUpdated ReviewVerdictOutcome = "updated"
	// ReviewVerdictUnchanged means a prior verdict comment already carried
	// exactly this body, so no forge write was performed. This is the steady
	// state on a re-reviewed PR whose head has not moved.
	ReviewVerdictUnchanged ReviewVerdictOutcome = "unchanged"
)

// AuditActionReviewVerdictPublished is recorded when the hive posts or edits a
// reviewer verdict comment on a PR. Unchanged cycles are NOT audited: they
// perform no write, and auditing them would bury the real publications.
const AuditActionReviewVerdictPublished = "review_verdict_published"

// PublishReviewVerdict posts body as the hive's verdict comment on repo#number,
// or edits the existing one in place. It never posts twice on the same PR.
//
// Idempotency is structural rather than remembered: the body rendered by
// review.RenderVerdictComment is deterministic and embeds the head SHA, so
// re-publishing an unchanged verdict produces a byte-identical body and this
// function skips the write entirely. A re-review at a NEW head renders a
// different body and edits the same comment, which is exactly the "updates
// rather than accumulates" requirement.
//
// The comment is adopted only if it carries review.VerdictCommentMarker AND —
// when the client knows its own App bot login — was authored by that bot. A
// client with no known bot login (a PAT client, or a test client) falls back
// to a marker-only match, mirroring findDigestCommentDetail.
func (c *Client) PublishReviewVerdict(ctx context.Context, repo string, number int, body string) (ReviewVerdictOutcome, error) {
	if c == nil {
		return "", ErrNoGitHubClient
	}
	if number <= 0 {
		return "", fmt.Errorf("publishing review verdict: invalid PR number %d", number)
	}
	owner, name := c.splitRepo(repo)

	// A verdict body is agent-sourced prose. It goes through the same two
	// guards every other agent-sourced write path uses: mentions are
	// neutralised so a finding quoting "@someone" cannot re-notify that human
	// on every re-render, and the canary scan honours the fail-closed
	// contract (kubestellar/hive#4960).
	body = advisory.NeutralizeMentions(body)
	if leak, ok := c.scanCanaryText(body, "hive-review-verdict:"+owner+"/"+name); ok && c.canaryFailClosed {
		return "", fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	}
	body = truncateComment(logscrub.ScrubString(body), "Verdict")

	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return "", err
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), review.VerdictCommentMarker) {
			continue
		}
		if !c.ownsVerdictComment(cm) {
			continue
		}
		if cm.GetBody() == body {
			return ReviewVerdictUnchanged, nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, name, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return "", fmt.Errorf("editing review verdict comment on %s/%s#%d: %w", owner, name, number, err)
		}
		c.recordVerdictAudit(owner, name, number, ReviewVerdictUpdated)
		return ReviewVerdictUpdated, nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return "", fmt.Errorf("creating review verdict comment on %s/%s#%d: %w", owner, name, number, err)
	}
	c.recordVerdictAudit(owner, name, number, ReviewVerdictCreated)
	return ReviewVerdictCreated, nil
}

// ownsVerdictComment reports whether the hive may edit this comment. When the
// App bot login is known, only the bot's own comments are adoptable, so a
// human who pastes the marker cannot make the hive overwrite their words.
func (c *Client) ownsVerdictComment(cm *gh.IssueComment) bool {
	login := strings.TrimSpace(c.appBotLogin)
	if login == "" {
		return true
	}
	return strings.EqualFold(safeGetLogin(cm.GetUser()), login)
}

func (c *Client) recordVerdictAudit(owner, name string, number int, outcome ReviewVerdictOutcome) {
	c.recordCreationAudit(AuditActionReviewVerdictPublished, InvocationMeta{Agent: AttributionAgentGovernor},
		"repo", owner+"/"+name,
		"number", strconv.Itoa(number),
		"outcome", string(outcome),
		"flow", "review-verdict")
	if c.logger != nil {
		c.logger.Info("review verdict published",
			slog.String("repo", owner+"/"+name),
			slog.Int("pr", number),
			slog.String("outcome", string(outcome)))
	}
}
