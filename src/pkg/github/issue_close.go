package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

var ErrReporterConfirmationRequired = errors.New("reporter confirmation required before closing human-filed bug issue")

type IssueCloseOptions struct {
	OverrideReason string
}

func ReporterConfirmationCloseGateReason(issue *gh.Issue) string {
	return humanFiledBugReason(issue)
}

func (c *Client) CloseIssue(ctx context.Context, repo string, number int, opts IssueCloseOptions) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return fmt.Errorf("CloseIssue: %w", err)
	}
	if number <= 0 {
		return fmt.Errorf("CloseIssue: issue number is required")
	}

	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		return fmt.Errorf("reading issue %s/%s#%d before close: %w", owner, repoName, number, err)
	}
	reason := ReporterConfirmationCloseGateReason(issue)
	overrideReason := strings.TrimSpace(opts.OverrideReason)
	if reason != "" && overrideReason == "" {
		body := reporterConfirmationRequestComment(issue)
		if _, _, commentErr := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)}); commentErr != nil {
			return fmt.Errorf("%w: %s; additionally failed to post confirmation request: %v", ErrReporterConfirmationRequired, reason, commentErr)
		}
		c.warn("issue close blocked pending reporter confirmation",
			"repo", owner+"/"+repoName,
			"issue", number,
			"reason", reason)
		return fmt.Errorf("%w: %s", ErrReporterConfirmationRequired, reason)
	}
	if reason != "" {
		body := fmt.Sprintf("Reporter-confirmation close override used for this human-filed bug-family issue.\n\nReason: %s", overrideReason)
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return fmt.Errorf("posting reporter-confirmation override on %s/%s#%d: %w", owner, repoName, number, err)
		}
		c.warn("issue close override used",
			"repo", owner+"/"+repoName,
			"issue", number,
			"reason", reason,
			"override_reason", overrideReason)
	}

	if _, _, err := c.client.Issues.Edit(ctx, owner, repoName, number, &gh.IssueRequest{State: gh.Ptr("closed")}); err != nil {
		return fmt.Errorf("closing issue %s/%s#%d: %w", owner, repoName, number, err)
	}
	return nil
}

func reporterConfirmationRequestComment(issue *gh.Issue) string {
	reporter := strings.TrimSpace(issue.GetUser().GetLogin())
	if reporter != "" {
		reporter = " @" + reporter
	}
	return "Reporter-confirmation gate: this looks like a human-filed bug-family issue, so the hive is leaving it open until the reporter confirms the symptom is gone." +
		reporter + ", please confirm when you have verified the fix. A maintainer can still close deliberately by recording an explicit override reason."
}
