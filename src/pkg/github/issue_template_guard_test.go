package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The exact filing that motivated the guard, hivecommons/hive#7153.
const (
	issue7153Title = "[guide] <specific description of the documentation gap>"
	issue7153Body  = `## Documentation Gap\n\n<what is missing or incorrect>\n\n## Recommendation\n\n<what should be added>\n\n---\n*Filed by guide agent (ACMM L5 — hold-gated mode)*`
)

func TestLooksLikeUnfilledPlaceholder(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantSpan string
	}{
		// --- must be caught -------------------------------------------------
		{"issue 7153 title", issue7153Title, "<specific description of the documentation gap>"},
		{"issue 7153 body", issue7153Body, "<what is missing or incorrect>"},
		{"two words is enough", "describe <the bug> here", "<the bug>"},
		{"hand written placeholder", "Hello <your name here>", "<your name here>"},
		{"placeholder with punctuation", "<what should be added, if anything>", "<what should be added, if anything>"},

		// --- must NOT be caught: real markup and real prose -----------------
		{"bare html element", "<details>hidden</details>", ""},
		{"self closing break", "line one<br>line two", ""},
		{"html with attributes", `<img src="a.png" alt="a diagram">`, ""},
		{"anchor with href", `<a href="https://example.com">the docs</a>`, ""},
		{"markdown autolink", "see <https://example.com/a/b> for more", ""},
		{"go generics", "the signature is List<T> not List", ""},
		{"multi param generics", "Map<K, V> is the type", ""},
		{"arithmetic comparison", "holds when a < b and c > d in practice", ""},
		{"html comment", "<!-- hidden note about the bug -->", ""},
		{"summary element", "<summary>click to expand</summary>", ""},
		{"empty string", "", ""},
		{"ordinary prose", "The dashboard chat bubble covers the fleet numbers.", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := looksLikeUnfilledPlaceholder(tt.in)
			if tt.wantSpan == "" {
				if ok {
					t.Fatalf("looksLikeUnfilledPlaceholder(%q) = %q, true; want no match", tt.in, got)
				}
				return
			}
			if !ok {
				t.Fatalf("looksLikeUnfilledPlaceholder(%q) = _, false; want %q", tt.in, tt.wantSpan)
			}
			if got != tt.wantSpan {
				t.Errorf("looksLikeUnfilledPlaceholder(%q) = %q, want %q", tt.in, got, tt.wantSpan)
			}
		})
	}
}

func TestHasUninterpretedEscapes(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"issue 7153 body", issue7153Body, true},
		{"exactly two literal escapes", `one\ntwo\nthree`, true},
		{"single literal escape is prose", `separate the fields with \n`, false},
		{"real newlines win", "## Heading\n\nA real body.", false},
		{"discusses escapes but is well formed", "Use this:\n\n    a\\nb\\nc\n\nThat is all.", false},
		{"empty body", "", false},
		{"windows style literal", `one\r\ntwo\r\nthree`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUninterpretedEscapes(tt.body); got != tt.want {
				t.Errorf("hasUninterpretedEscapes(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}

func TestValidateIssueTemplateFilled(t *testing.T) {
	tests := []struct {
		name      string
		title     string
		body      string
		wantErr   bool
		wantParts []string
	}{
		{
			name:      "issue 7153 as filed",
			title:     issue7153Title,
			body:      issue7153Body,
			wantErr:   true,
			wantParts: []string{"placeholder", "specific description of the documentation gap"},
		},
		{
			name:      "placeholder only in body",
			title:     "[guide] the install guide omits the proxy step",
			body:      "## Gap\n\n<what is missing or incorrect>",
			wantErr:   true,
			wantParts: []string{"placeholder", "what is missing or incorrect"},
		},
		{
			name:      "escapes only",
			title:     "[guide] the install guide omits the proxy step",
			body:      `## Gap\n\nThe proxy step is undocumented.\n\n## Fix\n\nAdd it.`,
			wantErr:   true,
			wantParts: []string{`\n`, "--body-file"},
		},
		{
			name:    "a properly filled issue passes",
			title:   "[guide] the install guide omits the HTTPS proxy step",
			body:    "## Documentation Gap\n\nThe install guide never mentions HTTPS_PROXY, so air-gapped installs fail at the image pull.\n\n## Recommendation\n\nDocument it next to the registry credentials step.",
			wantErr: false,
		},
		{
			name:    "a body full of legitimate markup passes",
			title:   "🐛 bug: the chat bubble covers dashboard data",
			body:    "<details>\n<summary>screenshot</summary>\n\n<img src=\"shot.png\" alt=\"the bubble over a progress row\">\n</details>\n\nSee <https://example.com/run/1> and note that List<T> is unaffected.",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateIssueTemplateFilled(tt.title, tt.body)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateIssueTemplateFilled() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateIssueTemplateFilled() = nil, want an error")
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must mention %q so the agent can self-correct, got: %v", want, err)
				}
			}
		})
	}
}

// The guard has to run before any HTTP call, for the same reason
// validateRepoRef does: an unfillable request must not cost a dedupe lookup, a
// label call per label, and a create, then be retried on the watcher's backoff.
func TestCreateIssueRejectsUnfilledTemplateBeforeAnyAPICall(t *testing.T) {
	org, repo := "hivecommons", "hive"
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	res, err := c.CreateIssue(context.Background(), org+"/"+repo, issue7153Title, issue7153Body, []string{"documentation"})
	if err == nil {
		t.Fatal("CreateIssue must refuse an issue that is still a template")
	}
	if !strings.Contains(err.Error(), "CreateIssue:") {
		t.Errorf("error must be wrapped by CreateIssue, got: %v", err)
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error must name the placeholder problem, got: %v", err)
	}
	if res.Number != 0 || res.AlreadyExisted || res.RejectedTwin {
		t.Errorf("no issue may be reported as created or reused, got %+v", res)
	}
	if len(calls) != 0 {
		t.Errorf("guard must reject before any GitHub call, but made: %v", calls)
	}
}

func TestCreateIssueAcceptsAFilledTemplate(t *testing.T) {
	org, repo := "hivecommons", "hive"
	c := newTestClient(t, httptest.NewServer(http.NewServeMux()), org, []string{repo})
	// Only the guard is under test here: a properly filled request must get
	// PAST it and fail later, at the (unrouted) API, rather than being refused
	// outright. A guard that rejected everything would pass the test above.
	_, err := c.CreateIssue(context.Background(), org+"/"+repo,
		"[guide] the install guide omits the HTTPS proxy step",
		"## Documentation Gap\n\nHTTPS_PROXY is never mentioned.", nil)
	if err != nil && strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("a filled-in issue must not be refused by the template guard, got: %v", err)
	}
}
