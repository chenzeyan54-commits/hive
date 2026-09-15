package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// hiveTrailer is the exact byte sequence AppendTrailer stamps on every
// hive-mediated create. isHiveFiledIssue keys on the AttributionTrailerPrefix
// substring, so any body that carries it — trailer or full policy paste —
// qualifies as hive-filed. This is the single source of truth the sweep uses
// to distinguish agent output from a maintainer's issue.
const hiveTrailer = "\n\n" + AttributionTrailerPrefix + " scanner via test"

// TestCountTaskListBoxes covers every parser trap the sweep must survive:
// bullet variants, indentation, mixed case, and — the one that would silently
// close every prose issue that quoted the policy — fenced code blocks.
func TestCountTaskListBoxes(t *testing.T) {
	cases := []struct {
		name              string
		body              string
		wantChecked       int
		wantUnchecked     int
	}{
		{
			name: "all ticked",
			body: "- [x] one\n- [x] two\n- [X] three\n",
			wantChecked:   3,
			wantUnchecked: 0,
		},
		{
			name: "some unticked",
			body: "- [x] one\n- [ ] two\n- [ ] three\n",
			wantChecked:   1,
			wantUnchecked: 2,
		},
		{
			name: "no boxes at all",
			body: "This is a prose finding with no task list.\n",
			wantChecked:   0,
			wantUnchecked: 0,
		},
		{
			name: "nested and indented boxes count",
			body: "- [x] top\n  - [x] child\n    - [ ] grandchild\n",
			wantChecked:   2,
			wantUnchecked: 1,
		},
		{
			name: "asterisk and plus bullets count",
			body: "* [x] star\n+ [ ] plus\n- [x] dash\n",
			wantChecked:   2,
			wantUnchecked: 1,
		},
		{
			name: "boxes inside triple-backtick fence do NOT count",
			body: "- [x] real one\n\n```markdown\n- [ ] not a real box\n- [x] also not\n```\n\n- [x] real two\n",
			wantChecked:   2,
			wantUnchecked: 0,
		},
		{
			name: "boxes inside tilde fence do NOT count",
			body: "- [x] real\n~~~\n- [ ] fake\n~~~\n- [ ] real unticked\n",
			wantChecked:   1,
			wantUnchecked: 1,
		},
		{
			name: "boxes inside longer backtick fence do NOT count",
			body: "````\n- [ ] fake\n- [x] fake ticked\n````\n- [x] real\n",
			wantChecked:   1,
			wantUnchecked: 0,
		},
		{
			name: "malformed box (no space in brackets) is ignored",
			body: "- [] not a task\n- [x] real\n",
			wantChecked:   1,
			wantUnchecked: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checked, unchecked := countTaskListBoxes(tc.body)
			if checked != tc.wantChecked || unchecked != tc.wantUnchecked {
				t.Fatalf("countTaskListBoxes(%q) = (%d, %d); want (%d, %d)",
					tc.body, checked, unchecked, tc.wantChecked, tc.wantUnchecked)
			}
		})
	}
}

// taskListSweepFixture is one open issue on the wire.
type taskListSweepFixture struct {
	Number      int
	Body        string
	Labels      []string
	IsPR        bool
	AuthorLogin string
	AuthorType  string // "User" (default) or "Bot"
}

// taskListSweepServer stands up a repo mux that:
//   - serves the fixtures on GET /issues
//   - records POSTs to /issues/{n}/comments (the audit-comment sink)
//   - records PATCHes to /issues/{n} whose payload sets state=closed (the close)
//
// A single httptest server suffices — we only need to observe which numbers got
// commented on and which got closed.
func taskListSweepServer(t *testing.T, org, repo string, issues []taskListSweepFixture) (*httptest.Server, *[]int, *[]int) {
	t.Helper()
	mux := http.NewServeMux()

	commented := &[]int{}
	closed := &[]int{}

	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		wire := make([]map[string]any, 0, len(issues))
		for _, iss := range issues {
			labels := make([]map[string]any, 0, len(iss.Labels))
			for _, l := range iss.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			entry := map[string]any{
				"number": iss.Number,
				"body":   iss.Body,
				"state":  "open",
				"labels": labels,
			}
			authorType := iss.AuthorType
			if authorType == "" {
				authorType = "User"
			}
			entry["user"] = map[string]any{"login": iss.AuthorLogin, "type": authorType}
			if iss.IsPR {
				entry["pull_request"] = map[string]any{"url": fmt.Sprintf("/repos/%s/%s/pulls/%d", org, repo, iss.Number)}
			}
			wire = append(wire, entry)
		}
		_ = json.NewEncoder(w).Encode(wire)
	})

	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		// Paths look like /repos/org/repo/issues/<n> and /repos/org/repo/issues/<n>/comments.
		rest := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/", org, repo))
		parts := strings.SplitN(rest, "/", 2)
		var n int
		fmt.Sscanf(parts[0], "%d", &n)
		if len(parts) == 2 && parts[1] == "comments" && r.Method == "POST" {
			*commented = append(*commented, n)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": int64(1_000_000 + n)})
			return
		}
		if len(parts) == 1 && r.Method == "PATCH" {
			var payload struct {
				State *string `json:"state"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.State != nil && *payload.State == "closed" {
				*closed = append(*closed, n)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": n, "state": "closed"})
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})

	server := httptest.NewServer(mux)
	return server, commented, closed
}

// TestSweepCompletedTaskListIssues drives the end-to-end sweep against a mock
// GitHub. Every row of the table is one safety gate the sweep MUST enforce; a
// regression that closes a human-filed issue or a zero-box issue would fail its
// row immediately.
func TestSweepCompletedTaskListIssues(t *testing.T) {
	org, repo := "hivecommons", "hive"

	fixtures := []taskListSweepFixture{
		{
			Number:      101,
			Body:        "## Findings\n\n- [x] a\n- [x] b\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		},
		{
			Number:      102,
			Body:        "## Findings\n\n- [x] a\n- [ ] b\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		},
		{
			Number:      103,
			Body:        "This is a hive-filed prose finding with no task list.\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		},
		{
			Number:      104,
			Body:        "- [x] a\n- [x] b\n\n(no attribution trailer — a maintainer wrote this)",
			AuthorLogin: "some-maintainer",
		},
		{
			Number:      105,
			Body:        "- [x] a\n- [x] b\n" + hiveTrailer,
			Labels:      []string{"hold"},
			AuthorLogin: "hive-app[bot]",
		},
		{
			Number:      106,
			Body:        "- [x] top\n  - [x] child\n    - [x] grandchild\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		},
		{
			Number:      107,
			Body:        "```\n- [ ] fake\n- [ ] also fake\n```\n\n- [x] real\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		},
		// #108 — all ticked + Bot author + NO trailer. Bot authorship is a
		// second positive hive-authorship signal, so this MUST close.
		{
			Number:      108,
			Body:        "- [x] a\n- [x] b\n",
			AuthorLogin: "hive-app[bot]",
			AuthorType:  "Bot",
		},
		// #109 — critical human-safety case: all ticked + non-Bot User + NO
		// trailer. Fail-CLOSED means this MUST stay open. A regression here is
		// the one outcome we absolutely cannot ship — the sweep would be
		// closing maintainer-filed issues on their behalf.
		{
			Number:      109,
			Body:        "- [x] a\n- [x] b\n",
			AuthorLogin: "some-human",
			AuthorType:  "User",
		},
	}

	server, commented, closed := taskListSweepServer(t, org, repo, fixtures)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})

	var audited int32
	result, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{
		Audit: func(TaskListSweepEvent) { atomic.AddInt32(&audited, 1) },
	})
	if err != nil {
		t.Fatalf("SweepCompletedTaskListIssues: %v", err)
	}

	wantClosed := map[int]bool{101: true, 106: true, 107: true, 108: true}
	if len(*closed) != len(wantClosed) {
		t.Fatalf("closed=%v; want exactly %v", *closed, keys(wantClosed))
	}
	for _, n := range *closed {
		if !wantClosed[n] {
			t.Errorf("closed issue #%d that should NOT have been closed by the sweep", n)
		}
	}

	// Every close MUST be preceded by the audit comment; the sweep names itself
	// so a maintainer reading the timeline can attribute the action.
	for n := range wantClosed {
		found := false
		for _, c := range *commented {
			if c == n {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("issue #%d was closed with no audit comment", n)
		}
	}

	if got := int(audited); got != len(wantClosed) {
		t.Errorf("audit callback fired %d times; want %d", got, len(wantClosed))
	}
	if got := len(result.Closed); got != len(wantClosed) {
		t.Errorf("result.Closed len = %d; want %d", got, len(wantClosed))
	}
	if result.Seen == 0 {
		t.Errorf("result.Seen = 0; want > 0")
	}
	if result.Skipped == 0 {
		t.Errorf("result.Skipped = 0; every one of the ineligible fixtures should have been counted as skipped")
	}
}

// TestSweepCompletedTaskListIssues_RespectsMaxCloses proves the per-tick cap
// is a real bound and not just a suggestion. Ten eligible issues; MaxCloses=2;
// the sweep must close exactly two.
func TestSweepCompletedTaskListIssues_RespectsMaxCloses(t *testing.T) {
	org, repo := "hivecommons", "hive"
	fixtures := make([]taskListSweepFixture, 0, 10)
	for i := 0; i < 10; i++ {
		fixtures = append(fixtures, taskListSweepFixture{
			Number:      200 + i,
			Body:        "- [x] only box\n" + hiveTrailer,
			AuthorLogin: "hive-app[bot]",
		})
	}
	server, _, closed := taskListSweepServer(t, org, repo, fixtures)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.SweepCompletedTaskListIssues(context.Background(), TaskListSweepOptions{MaxCloses: 2})
	if err != nil {
		t.Fatalf("SweepCompletedTaskListIssues: %v", err)
	}
	if len(*closed) != 2 {
		t.Fatalf("closed = %v; want exactly 2", *closed)
	}
	if len(result.Closed) != 2 {
		t.Fatalf("result.Closed = %d; want 2", len(result.Closed))
	}
}

// TestIsHiveFiledIssue directly exercises the hive-filed classifier the sweep
// keys on. If this returns true for a human-filed issue the sweep will close
// it — this test guards the classifier against exactly that regression.
func TestIsHiveFiledIssue(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		authorType string
		want       bool
	}{
		{"nil body + non-Bot is not hive-filed", "", "User", false},
		{"body with trailer + non-Bot is hive-filed", "some finding\n\n" + AttributionTrailerPrefix + " scanner", "User", true},
		{"body without trailer + non-Bot is not hive-filed", "some finding without any hive stamp", "User", false},
		{"Bot author with no trailer is hive-filed", "no trailer here", "Bot", true},
		{"Bot author (lowercase 'bot') is hive-filed", "no trailer here", "bot", true},
		{"Bot author + trailer is hive-filed", "with trailer\n\n" + AttributionTrailerPrefix + " x", "Bot", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss := &gh.Issue{
				Body: gh.Ptr(tc.body),
				User: &gh.User{Type: gh.Ptr(tc.authorType)},
			}
			if got := isHiveFiledIssue(iss); got != tc.want {
				t.Errorf("isHiveFiledIssue(body=%q, type=%q) = %v; want %v", tc.body, tc.authorType, got, tc.want)
			}
		})
	}
}

func keys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
