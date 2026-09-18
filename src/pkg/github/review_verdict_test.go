package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// verdictForge is a minimal in-memory stand-in for a PR's comment thread. It
// records creates and edits separately, which is the whole point: #7469's
// idempotency requirement is that a re-review EDITS rather than ADDS.
type verdictForge struct {
	mu       sync.Mutex
	comments []map[string]any
	nextID   int64
	creates  int
	edits    int
}

func newVerdictForge(existing ...map[string]any) *verdictForge {
	f := &verdictForge{nextID: 100}
	f.comments = append(f.comments, existing...)
	return f
}

func (f *verdictForge) server(t *testing.T, owner, repo string, number int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	list := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number)
	mux.HandleFunc(list, func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(f.comments)
		case http.MethodPost:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.creates++
			f.nextID++
			cm := map[string]any{"id": f.nextID, "body": in["body"], "user": map[string]any{"login": "hive[bot]"}}
			f.comments = append(f.comments, cm)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(cm)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/", owner, repo), func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.edits++
		idStr := strings.TrimPrefix(r.URL.Path, fmt.Sprintf("/repos/%s/%s/issues/comments/", owner, repo))
		for i := range f.comments {
			if commentIDString(f.comments[i]["id"]) == idStr {
				f.comments[i]["body"] = in["body"]
			}
		}
		id, _ := strconv.ParseInt(idStr, 10, 64)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "body": in["body"], "user": map[string]any{"login": "hive[bot]"}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// commentIDString renders a stored comment id (float64 from a literal, int64
// from a create) as the decimal string the edit path carries in its URL.
func commentIDString(v any) string {
	switch id := v.(type) {
	case float64:
		return strconv.FormatInt(int64(id), 10)
	case int64:
		return strconv.FormatInt(id, 10)
	default:
		return ""
	}
}

func (f *verdictForge) counts() (creates, edits, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.edits, len(f.comments)
}

func (f *verdictForge) bodyAt(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i >= len(f.comments) {
		return ""
	}
	b, _ := f.comments[i]["body"].(string)
	return b
}

func sampleVerdictBody(headSHA string) string {
	return review.RenderVerdictComment(review.Aggregate{
		Repo:         "testorg/testrepo",
		Number:       7,
		HeadSHA:      headSHA,
		Verdict:      review.VerdictRequiresHuman,
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictRequiresHuman},
	})
}

// TestPublishReviewVerdict_CreatesThenIsIdempotent is #7469 requirement (b):
// publishing the SAME verdict for the SAME head SHA twice must leave exactly
// one comment and perform no second write.
func TestPublishReviewVerdict_CreatesThenIsIdempotent(t *testing.T) {
	org, repo := "testorg", "testrepo"
	f := newVerdictForge()
	c := newTestClient(t, f.server(t, org, repo, 7), org, []string{repo})

	body := sampleVerdictBody("sha-one")
	got, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, body)
	if err != nil {
		t.Fatalf("first publish: %v", err)
	}
	if got != ReviewVerdictCreated {
		t.Fatalf("first publish outcome = %q, want %q", got, ReviewVerdictCreated)
	}

	got, err = c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, body)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if got != ReviewVerdictUnchanged {
		t.Fatalf("second publish outcome = %q, want %q", got, ReviewVerdictUnchanged)
	}
	creates, edits, total := f.counts()
	if creates != 1 {
		t.Errorf("creates = %d, want 1 — a re-publish must never post a second comment", creates)
	}
	if edits != 0 {
		t.Errorf("edits = %d, want 0 — an unchanged body must not be rewritten", edits)
	}
	if total != 1 {
		t.Errorf("comment count = %d, want 1", total)
	}
}

// TestPublishReviewVerdict_NewHeadSHAEditsInPlace is the other half of
// idempotency: a re-review at a new head must UPDATE the existing comment, not
// accumulate a second one.
func TestPublishReviewVerdict_NewHeadSHAEditsInPlace(t *testing.T) {
	org, repo := "testorg", "testrepo"
	f := newVerdictForge()
	c := newTestClient(t, f.server(t, org, repo, 7), org, []string{repo})

	if _, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, sampleVerdictBody("sha-one")); err != nil {
		t.Fatalf("first publish: %v", err)
	}
	got, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, sampleVerdictBody("sha-two"))
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if got != ReviewVerdictUpdated {
		t.Fatalf("outcome = %q, want %q", got, ReviewVerdictUpdated)
	}
	creates, edits, total := f.counts()
	if creates != 1 || edits != 1 || total != 1 {
		t.Errorf("creates=%d edits=%d total=%d; want 1/1/1 — a new head SHA must edit the existing comment", creates, edits, total)
	}
	if !strings.Contains(f.bodyAt(0), review.VerdictSHAMarker("sha-two")) {
		t.Errorf("edited comment does not carry the new head SHA marker: %q", f.bodyAt(0))
	}
}

// TestPublishReviewVerdict_IgnoresForeignMarkerComment: a human (or another
// bot) who pastes the marker must not be overwritten by the hive when the
// client knows its own App bot identity.
func TestPublishReviewVerdict_IgnoresForeignMarkerComment(t *testing.T) {
	org, repo := "testorg", "testrepo"
	f := newVerdictForge(map[string]any{
		"id":   float64(55),
		"body": review.VerdictCommentMarker + "\nsomething a human wrote",
		"user": map[string]any{"login": "mallory"},
	})
	c := newTestClient(t, f.server(t, org, repo, 7), org, []string{repo})
	c.appBotLogin = "hive[bot]"

	got, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, sampleVerdictBody("sha-one"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got != ReviewVerdictCreated {
		t.Fatalf("outcome = %q, want %q", got, ReviewVerdictCreated)
	}
	creates, edits, _ := f.counts()
	if edits != 0 {
		t.Errorf("edits = %d, want 0 — the hive must not overwrite a foreign comment", edits)
	}
	if creates != 1 {
		t.Errorf("creates = %d, want 1", creates)
	}
	if !strings.Contains(f.bodyAt(0), "something a human wrote") {
		t.Errorf("the foreign comment was clobbered: %q", f.bodyAt(0))
	}
}

// TestPublishReviewVerdict_AdoptsOwnBotComment is the positive control for the
// authorship check: the App bot's own prior marker comment IS adopted.
func TestPublishReviewVerdict_AdoptsOwnBotComment(t *testing.T) {
	org, repo := "testorg", "testrepo"
	f := newVerdictForge(map[string]any{
		"id":   float64(55),
		"body": sampleVerdictBody("older-sha"),
		"user": map[string]any{"login": "hive[bot]"},
	})
	c := newTestClient(t, f.server(t, org, repo, 7), org, []string{repo})
	c.appBotLogin = "hive[bot]"

	got, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7, sampleVerdictBody("newer-sha"))
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got != ReviewVerdictUpdated {
		t.Fatalf("outcome = %q, want %q", got, ReviewVerdictUpdated)
	}
	creates, edits, total := f.counts()
	if creates != 0 || edits != 1 || total != 1 {
		t.Errorf("creates=%d edits=%d total=%d; want 0/1/1", creates, edits, total)
	}
}

// TestPublishReviewVerdict_NeutralizesMentions: a finding quoting a username
// must not re-notify that human on every re-render.
func TestPublishReviewVerdict_NeutralizesMentions(t *testing.T) {
	org, repo := "testorg", "testrepo"
	f := newVerdictForge()
	c := newTestClient(t, f.server(t, org, repo, 7), org, []string{repo})

	if _, err := c.PublishReviewVerdict(context.Background(), org+"/"+repo, 7,
		review.VerdictCommentMarker+"\nfiled by @someone earlier"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if strings.Contains(f.bodyAt(0), "@someone") {
		t.Errorf("published body still carries a raw @mention: %q", f.bodyAt(0))
	}
}

func TestPublishReviewVerdict_RejectsInvalidNumber(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NewServeMux()), "o", []string{"r"})
	if _, err := c.PublishReviewVerdict(context.Background(), "o/r", 0, "body"); err == nil {
		t.Fatal("expected an error for PR number 0")
	}
}

func TestPublishReviewVerdict_NilClient(t *testing.T) {
	var c *Client
	if _, err := c.PublishReviewVerdict(context.Background(), "o/r", 1, "body"); err != ErrNoGitHubClient {
		t.Fatalf("err = %v, want %v", err, ErrNoGitHubClient)
	}
}
