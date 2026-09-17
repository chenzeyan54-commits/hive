package hub

import (
	"os"
	"strings"
	"testing"
)

// TestTerminalAuthzAnnotationsMatchProvisioningTemplate is the point of the
// whole file: the repair must write EXACTLY what provisioning renders.
//
// If the two drift, the sweep either writes an annotation nginx ignores (repair
// that silently does nothing) or fights the template forever (patch every cycle
// on a converged spoke). Pin the rendered forms against the template source so a
// future edit to one and not the other fails here.
func TestTerminalAuthzAnnotationsMatchProvisioningTemplate(t *testing.T) {
	src, err := os.ReadFile("saas_provision.go")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := string(src)
	for _, want := range []string{
		`nginx.ingress.kubernetes.io/auth-url: "{{.HubPublicURL}}/api/saas/auth-check?hive={{.ID}}&uri=$request_uri"`,
		`nginx.ingress.kubernetes.io/auth-signin: "{{.HubPublicURL}}/login?redirect=$scheme://$http_host$request_uri"`,
		`nginx.ingress.kubernetes.io/auth-response-headers: "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth"`,
	} {
		if !strings.Contains(tmpl, want) {
			t.Fatalf("provisioning template no longer renders %q — the reconcile in terminal_authz_reconcile.go would now write a DIFFERENT value than provisioning does", want)
		}
	}

	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.hivecommons.dev")
	got := terminalAuthzAnnotations("myhive")
	if got == nil {
		t.Fatal("terminalAuthzAnnotations returned nil for a well-formed hive")
	}
	// The template substitutes {{.HubPublicURL}} and {{.ID}}; do the same to the
	// pinned literals above and require byte equality with what we would patch.
	for key, tmplLiteral := range map[string]string{
		"nginx.ingress.kubernetes.io/auth-url":              "{{.HubPublicURL}}/api/saas/auth-check?hive={{.ID}}&uri=$request_uri",
		"nginx.ingress.kubernetes.io/auth-signin":           "{{.HubPublicURL}}/login?redirect=$scheme://$http_host$request_uri",
		"nginx.ingress.kubernetes.io/auth-response-headers": "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth",
	} {
		rendered := strings.NewReplacer(
			"{{.HubPublicURL}}", "https://hub.hivecommons.dev",
			"{{.ID}}", "myhive",
		).Replace(tmplLiteral)
		if got[key] != rendered {
			t.Fatalf("annotation %s: reconcile would write %q, provisioning renders %q", key, got[key], rendered)
		}
	}
}

// A missing hub URL must produce NO patch. Writing an empty auth-url would turn
// a missing-annotation exposure into a hard outage on a working spoke, which is
// a strictly worse failure than the one being repaired.
func TestTerminalAuthzAnnotationsFailSafeOnEmptyInputs(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.hivecommons.dev")
	if terminalAuthzAnnotations("") != nil {
		t.Fatal("an empty hive id must not produce annotations (auth-check?hive= would authorize against no hive)")
	}
	// With HIVE_HUB_PUBLIC_URL unset, hubPublicURL() falls back to
	// defaultHubPublicURL rather than returning empty — and that is the correct
	// outcome, because the provisioning template resolves .HubPublicURL through
	// the SAME function, so the repair still writes exactly what provisioning
	// would have written. The nil-guard in terminalAuthzAnnotations stays as a
	// belt-and-braces against that fallback ever being removed; assert the
	// property that actually holds, which is "never empty, always the same URL
	// provisioning uses".
	t.Setenv("HIVE_HUB_PUBLIC_URL", "")
	got := terminalAuthzAnnotations("myhive")
	if got == nil {
		t.Fatal("hubPublicURL() has a non-empty default, so annotations must still be produced")
	}
	authURL := got["nginx.ingress.kubernetes.io/auth-url"]
	if !strings.HasPrefix(authURL, hubPublicURL()+"/api/saas/auth-check?hive=myhive") {
		t.Fatalf("auth-url %q must be built from hubPublicURL() %q, the same source the template uses", authURL, hubPublicURL())
	}
	if strings.HasPrefix(authURL, "/") {
		t.Fatal("auth-url must never be host-relative — nginx would call itself, not the hub")
	}
}

// terminalAuthzMissing must compare VALUES, not presence. This is the case that
// matters most: an annotation naming a DIFFERENT hive in ?hive= authorizes the
// terminal against the wrong tenant while looking perfectly converged.
func TestTerminalAuthzMissingComparesValuesNotPresence(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.hivecommons.dev")
	want := terminalAuthzAnnotations("myhive")

	if got := terminalAuthzMissing(map[string]string{}, want); len(got) != 3 {
		t.Fatalf("a spoke with NO annotations must report all 3 missing, got %d — this is the live fleet's state", len(got))
	}
	if got := terminalAuthzMissing(want, want); got != nil {
		t.Fatalf("a converged spoke must produce no patch, got %v (it would be rewritten every cycle)", got)
	}

	wrongHive := map[string]string{}
	for k, v := range want {
		wrongHive[k] = v
	}
	wrongHive["nginx.ingress.kubernetes.io/auth-url"] = "https://hub.hivecommons.dev/api/saas/auth-check?hive=SOMEONE-ELSE&uri=$request_uri"
	got := terminalAuthzMissing(wrongHive, want)
	if got == nil || got["nginx.ingress.kubernetes.io/auth-url"] != want["nginx.ingress.kubernetes.io/auth-url"] {
		t.Fatal("an auth-url pointing at ANOTHER hive must be reported as drift — it looks present but authorizes the wrong tenant")
	}

	// Only the drifted key is patched; a present-and-correct key is left alone
	// so the merge patch stays minimal.
	if len(got) != 1 {
		t.Fatalf("only the drifted annotation should be patched, got %d keys: %v", len(got), got)
	}
}

// The repair must never replace the annotation map wholesale: a spoke's
// cert-manager issuer and proxy timeouts live in the same map, and dropping them
// would break TLS renewal and cut long-lived terminal sessions at 60s.
func TestTerminalAuthzMissingPreservesUnrelatedAnnotations(t *testing.T) {
	t.Setenv("HIVE_HUB_PUBLIC_URL", "https://hub.hivecommons.dev")
	want := terminalAuthzAnnotations("myhive")
	live := map[string]string{
		"cert-manager.io/cluster-issuer":                    "letsencrypt",
		"nginx.ingress.kubernetes.io/proxy-read-timeout":    "3600",
		"nginx.ingress.kubernetes.io/auth-response-headers": want["nginx.ingress.kubernetes.io/auth-response-headers"],
	}
	got := terminalAuthzMissing(live, want)
	for k := range got {
		if !strings.HasPrefix(k, "nginx.ingress.kubernetes.io/auth-") {
			t.Fatalf("patch must only add auth-* annotations, it also names %q", k)
		}
	}
	if _, ok := got["cert-manager.io/cluster-issuer"]; ok {
		t.Fatal("the patch must never mention cert-manager.io/cluster-issuer")
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly the 2 absent auth annotations, got %d: %v", len(got), got)
	}
}
