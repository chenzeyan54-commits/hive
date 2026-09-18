package main

// Tests for the verdict-publisher wiring in cmd/hive (hivecommons/hive#7469):
// publishReviewVerdicts and the refreshReviewVerdicts gate that feeds it.
//
// The design property under test is the one the issue is built around: the
// reviewer agent holds no GitHub write access, so the WRITE must happen here,
// in the spoke, and only when the operator has explicitly opted in. The
// "feature off ⇒ zero GitHub traffic" test below is the executable form of
// that contract — any request reaching the fake forge fails the test.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

// publishForge is a fake GitHub comment endpoint that records every request it
// sees, so a test can assert both "nothing was written" and "exactly one
// comment was written".
type publishForge struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
	creates  int
	edits    int
}

func (f *publishForge) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			out := make([]map[string]any, 0, len(f.bodies))
			for i, b := range f.bodies {
				out = append(out, map[string]any{"id": int64(100 + i), "body": b, "user": map[string]any{"login": "hive[bot]"}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			body, _ := in["body"].(string)
			f.bodies = append(f.bodies, body)
			f.creates++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": int64(100 + len(f.bodies) - 1), "body": body})
		case r.Method == http.MethodPatch:
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			body, _ := in["body"].(string)
			if len(f.bodies) > 0 {
				f.bodies[0] = body
			}
			f.edits++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": int64(100), "body": body})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (f *publishForge) snapshot() (requests []string, creates, edits int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), f.creates, f.edits
}

// writeVerdictArtifact writes a review-verdicts.json holding one aggregate for
// hivecommons/hive#4321 at headSHA, with one cited finding.
func writeVerdictArtifact(t *testing.T, headSHA string) review.Artifact {
	t.Helper()
	art := review.Artifact{Items: []review.Aggregate{{
		Repo:         "hivecommons/hive",
		Number:       4321,
		HeadSHA:      headSHA,
		Verdict:      review.VerdictRequiresHuman,
		Perspectives: map[review.Perspective]review.Verdict{review.PerspectiveCorrectness: review.VerdictRequiresHuman},
		Findings: []review.PerspectiveFinding{{
			Perspective: review.PerspectiveCorrectness,
			Finding: outputschema.Finding{
				Title:    "unguarded nil dereference",
				Severity: outputschema.SeverityHigh,
				Summary:  "cfg may be nil here",
				File:     "src/cmd/hive/main.go",
				Line:     9147,
			},
		}},
	}}}
	if err := review.WriteArtifact("", art); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return art
}

func publishEnabledConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Org: "hivecommons"},
		Review:  config.ReviewConfig{PublishVerdicts: true},
	}
}

// TestPublishReviewVerdicts_OffByDefaultWritesNothing is #7469 requirement
// (a). The zero-value ReviewConfig must produce NO GitHub traffic at all —
// not a read, not a write. Anything else would mean the hive started
// commenting on an operator's repositories without being asked.
func TestPublishReviewVerdicts_OffByDefaultWritesNothing(t *testing.T) {
	redirectReviewPaths(t)
	art := writeVerdictArtifact(t, reviewTestSHA)
	forge := &publishForge{}
	client := github.NewClientForTest(forge.server(t).URL, "hivecommons", []string{"hive"}, restoreTestLogger())

	// The default config: PublishVerdicts is the zero value, false.
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons"}}
	publishReviewVerdicts(context.Background(), cfg, client, actionableWithPR("hive-bot"), art, restoreTestLogger())

	reqs, creates, edits := forge.snapshot()
	if len(reqs) != 0 {
		t.Fatalf("publishing is off by default but the forge saw %d request(s): %v", len(reqs), reqs)
	}
	if creates != 0 || edits != 0 {
		t.Fatalf("creates=%d edits=%d; want 0/0 when the feature is off", creates, edits)
	}
}

// TestPublishReviewVerdicts_PublishesFindingsWithCitations is requirement (c)
// at the wiring level: with the feature ON, exactly one comment is posted and
// it carries the finding text and its file:line citation.
func TestPublishReviewVerdicts_PublishesFindingsWithCitations(t *testing.T) {
	redirectReviewPaths(t)
	art := writeVerdictArtifact(t, reviewTestSHA)
	forge := &publishForge{}
	client := github.NewClientForTest(forge.server(t).URL, "hivecommons", []string{"hive"}, restoreTestLogger())

	publishReviewVerdicts(context.Background(), publishEnabledConfig(), client, actionableWithPR("hive-bot"), art, restoreTestLogger())

	_, creates, edits := forge.snapshot()
	if creates != 1 || edits != 0 {
		t.Fatalf("creates=%d edits=%d; want 1/0", creates, edits)
	}
	body := forge.bodies[0]
	for _, want := range []string{
		"unguarded nil dereference",
		"src/cmd/hive/main.go:9147",
		"correctness",
		review.VerdictSHAMarker(reviewTestSHA),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("published comment is missing %q\n---\n%s", want, body)
		}
	}
}

// TestPublishReviewVerdicts_SecondRunSameHeadSHADoesNotAddAComment is
// requirement (b) at the wiring level.
func TestPublishReviewVerdicts_SecondRunSameHeadSHADoesNotAddAComment(t *testing.T) {
	redirectReviewPaths(t)
	art := writeVerdictArtifact(t, reviewTestSHA)
	forge := &publishForge{}
	client := github.NewClientForTest(forge.server(t).URL, "hivecommons", []string{"hive"}, restoreTestLogger())
	cfg := publishEnabledConfig()

	publishReviewVerdicts(context.Background(), cfg, client, actionableWithPR("hive-bot"), art, restoreTestLogger())
	publishReviewVerdicts(context.Background(), cfg, client, actionableWithPR("hive-bot"), art, restoreTestLogger())

	_, creates, edits := forge.snapshot()
	if creates != 1 {
		t.Errorf("creates = %d, want 1 — a second publish at the same head SHA must not add a comment", creates)
	}
	if edits != 0 {
		t.Errorf("edits = %d, want 0 — an unchanged verdict must not be rewritten", edits)
	}
	if len(forge.bodies) != 1 {
		t.Errorf("comment count = %d, want 1", len(forge.bodies))
	}
}

// TestPublishReviewVerdicts_StaleHeadSHAIsNotPublished: a verdict for a
// superseded commit must never be presented to a human as current.
func TestPublishReviewVerdicts_StaleHeadSHAIsNotPublished(t *testing.T) {
	redirectReviewPaths(t)
	art := writeVerdictArtifact(t, "0000000000000000000000000000000000000000")
	forge := &publishForge{}
	client := github.NewClientForTest(forge.server(t).URL, "hivecommons", []string{"hive"}, restoreTestLogger())

	// actionableWithPR reports the PR at reviewTestSHA, which is not the head
	// the artifact's verdict judged.
	publishReviewVerdicts(context.Background(), publishEnabledConfig(), client, actionableWithPR("hive-bot"), art, restoreTestLogger())

	reqs, creates, _ := forge.snapshot()
	if creates != 0 {
		t.Fatalf("a stale verdict was published (creates=%d)", creates)
	}
	if len(reqs) != 0 {
		t.Fatalf("a stale verdict still cost GitHub traffic: %v", reqs)
	}
}

func TestPublishReviewVerdicts_NilInputsAreNoOps(t *testing.T) {
	redirectReviewPaths(t)
	art := writeVerdictArtifact(t, reviewTestSHA)
	forge := &publishForge{}
	client := github.NewClientForTest(forge.server(t).URL, "hivecommons", []string{"hive"}, restoreTestLogger())
	logger := restoreTestLogger()

	publishReviewVerdicts(context.Background(), nil, client, actionableWithPR("hive-bot"), art, logger)
	publishReviewVerdicts(context.Background(), publishEnabledConfig(), nil, actionableWithPR("hive-bot"), art, logger)
	publishReviewVerdicts(context.Background(), publishEnabledConfig(), client, nil, art, logger)
	publishReviewVerdicts(context.Background(), publishEnabledConfig(), client, actionableWithPR("hive-bot"), review.Artifact{}, logger)

	if reqs, _, _ := forge.snapshot(); len(reqs) != 0 {
		t.Fatalf("nil/empty inputs produced GitHub traffic: %v", reqs)
	}
}

// TestRefreshReviewVerdicts_PublishVerdictsAloneRefreshesArtifact is the
// reason refreshReviewVerdicts gained a second gate: the spokes that need the
// publisher are exactly the ones that do NOT enable the merge gate, so
// require_approval alone would leave the artifact permanently stale.
func TestRefreshReviewVerdicts_PublishVerdictsAloneRefreshesArtifact(t *testing.T) {
	dir := redirectReviewPaths(t)
	report := review.PerspectiveReport{
		AgentReport: outputschema.AgentReport{
			Lane:       "review-swarm",
			Kind:       outputschema.KindReview,
			Findings:   []outputschema.Finding{},
			PRsOpened:  []outputschema.PROpened{},
			BeadsFiled: []outputschema.BeadFiled{},
			Summary:    "correctness review summary",
		},
		Perspective: review.PerspectiveCorrectness,
		Verdict:     review.VerdictApprove,
		Repo:        "hivecommons/hive",
		Number:      4321,
		HeadSHA:     reviewTestSHA,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	reportPath := filepath.Join(dir, review.ReviewReportFilePrefix+"correctness"+review.ReviewReportFileSuffix)
	if err := os.WriteFile(reportPath, raw, 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}

	cfg := &config.Config{Review: config.ReviewConfig{PublishVerdicts: true}}
	art := refreshReviewVerdicts(cfg, restoreTestLogger())
	if len(art.Items) != 1 {
		t.Fatalf("refreshReviewVerdicts returned %d aggregates, want 1 — publish_verdicts alone must drive collection", len(art.Items))
	}
	if _, err := os.Stat(review.ReviewVerdictsPath); err != nil {
		t.Fatalf("artifact was not written: %v", err)
	}
}

// TestRefreshReviewVerdicts_BothGatesOffStillWritesNothing is the negative
// control for the widened gate: adding publish_verdicts must not have turned
// collection on for everyone.
func TestRefreshReviewVerdicts_BothGatesOffStillWritesNothing(t *testing.T) {
	redirectReviewPaths(t)
	art := refreshReviewVerdicts(&config.Config{}, restoreTestLogger())
	if len(art.Items) != 0 {
		t.Fatalf("returned %d aggregates with both gates off, want 0", len(art.Items))
	}
	if _, err := os.Stat(review.ReviewVerdictsPath); !os.IsNotExist(err) {
		t.Fatalf("artifact written with both gates off (stat err=%v)", err)
	}
}
