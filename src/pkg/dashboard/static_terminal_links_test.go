package dashboard

// Moved from static_index_test.go when the IndexDocument unit tests followed
// the type into pkg/dashboard/webstatic (#5565). This test pins the CONTENT of
// the embedded SPA document, which stays owned by pkg/dashboard (static/), so
// it stays here with it.

import (
	"os"
	"strings"
	"testing"
)

func TestStaticTerminalLinksRenewAssertionBeforeOpening(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const TERMINAL_ASSERTION_RENEW_PATH = '/api/terminal/assertion/renew';",
		"async function renewTerminalAssertion()",
		"credentials: 'same-origin'",
		"case 'openTerminal': e.preventDefault(); openTerminal(agent, el.href); break;",
		"data-action=\"openTerminal\"",
		"openTerminal(name);",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard terminal renewal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "window.open(terminalUrl(name), '_blank', 'noopener');") {
		t.Fatal("welcome terminal action still opens /terminal directly without renewing the assertion")
	}
}

// TestStaticTerminalHostedApexWiring pins the two hosted-hive terminal defects
// that shipped alongside the proxy's single-apex /terminal gate.
//
//  1. isHostedHiveHost() suffix-matched ONE hardcoded apex, so on the rebranded
//     hive.hivecommons.dev fleet renewTerminalAssertion() returned early, the
//     15-minute hive_terminal_assertion expired with nothing to refresh it, and
//     ttyd's next reconnect was rejected.
//  2. dashboardTokenConfigured() mapped EVERY non-OK response to "a shared token
//     IS configured". A hosted hive has no shared token and answers
//     /api/auth/token with 404, so when the handoff failed openTerminal() took
//     the tab.close() branch instead of falling back to the plain terminal URL —
//     the operator sees a tab flash open and vanish.
func TestStaticTerminalHostedApexWiring(t *testing.T) {
	body, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	for _, want := range []string{
		"const HOSTED_HIVE_HOST_SUFFIXES = ['.hive.kubestellar.io', '.hive.hivecommons.dev'];",
		"HOSTED_HIVE_HOST_SUFFIXES.some(suffix => host.endsWith(suffix))",
		"if (r.status === 404) return false;",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("static dashboard hosted-hive terminal wiring missing %q", want)
		}
	}
	if strings.Contains(html, "endsWith('.hive.kubestellar.io')") {
		t.Fatal("isHostedHiveHost still hardcodes a single hosted apex; assertion renewal no-ops on every other apex")
	}
}
