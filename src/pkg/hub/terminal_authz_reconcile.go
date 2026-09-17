package hub

// Terminal-Ingress annotation drift repair.
//
// WHY THIS FILE EXISTS. The provisioning template renders the three
// nginx auth annotations onto the hive-terminal Ingress, and
// saas_provision_terminal_authz_test.go asserts they are present — so the
// TEMPLATE has been correct. The DEPLOYED fleet had nonetheless drifted: every
// hosted spoke checked was missing auth-url entirely, i.e. the ingress-level
// per-hive authorization (CWE-862, finding C3) was simply absent in production.
//
// The reason the drift was permanent is that NOTHING ever repaired it. The only
// post-provision mutation of a spoke's Ingress is the vanity-host attach loop in
// saas_provision.go, which appends spec.rules entries and TLS hosts and never
// reads or writes metadata.annotations. A spoke provisioned before the
// annotations were added — or one whose Ingress was ever recreated from an older
// manifest — stayed missing them forever, because re-provisioning is not part of
// any normal lifecycle.
//
// SECONDARY DEFENCE, STATED PLAINLY. This is defense in depth, NOT the
// containment. The proxy's own /terminal gate (src/proxy/server.js) is what
// actually protects an exposed spoke, because it ships inside the container
// image and therefore reaches an already-running spoke on its next restart with
// no hub action and no config change. This sweep only closes the outer layer,
// and only for spokes the hub can still reach. Never rely on it as the reason a
// terminal is safe.
//
// Modelled deliberately on reconcilePerHiveEnv: same eligibility filter, same
// cluster-reachability breaker, same bounded kubectl timeout, same
// small-patches-per-cycle cap, same "idempotent, non-fatal, skip and log"
// posture. A converged spoke produces no patch and therefore no churn.

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

const (
	// terminalAuthzReconcileInterval throttles the sweep. Annotation drift is a
	// standing condition, not an event: it does not appear between ticks, so
	// there is nothing to gain from sweeping faster than the env lane.
	terminalAuthzReconcileInterval = 15 * time.Minute

	// terminalAuthzKubectlTimeout bounds each per-hive kubectl get/patch so one
	// wedged API server cannot stall the whole sweep.
	terminalAuthzKubectlTimeout = 15 * time.Second

	// terminalAuthzMaxPatchesPerCycle caps how many spokes may be PATCHED per
	// cycle. Patching an Ingress does not restart a pod, but it does make the
	// ingress controller reload, so a fleet-wide simultaneous rewrite is still
	// worth avoiding. A converged fleet patches nothing at all.
	terminalAuthzMaxPatchesPerCycle = 3
)

// terminalAuthzAnnotations returns the annotations the hive-terminal Ingress
// MUST carry for hive id, keyed exactly as the provisioning template renders
// them. Keep this in lockstep with the hive-terminal Ingress block in
// saas_provision.go — terminal_authz_reconcile_test.go pins that they agree, so
// a future edit to one and not the other fails rather than silently reintroduces
// the drift this file exists to repair.
//
// Returns nil when the hub has no public URL to point auth-url at. That is the
// fail-safe: a request to an empty auth-url does not "fail open to the hub", it
// produces a broken Ingress, so writing it would turn a missing-annotation
// exposure into an outage. Skip and log instead.
func terminalAuthzAnnotations(hiveID string) map[string]string {
	hub := strings.TrimRight(strings.TrimSpace(hubPublicURL()), "/")
	id := strings.TrimSpace(hiveID)
	if hub == "" || id == "" {
		return nil
	}
	return map[string]string{
		"nginx.ingress.kubernetes.io/auth-url":              hub + "/api/saas/auth-check?hive=" + id + "&uri=$request_uri",
		"nginx.ingress.kubernetes.io/auth-signin":           hub + "/login?redirect=$scheme://$http_host$request_uri",
		"nginx.ingress.kubernetes.io/auth-response-headers": "X-Hive-User,X-Hive-Role,X-Hive-Proxy-Auth",
	}
}

// terminalAuthzMissing returns the subset of want that live does not already
// carry with the identical value.
//
// Compares VALUES, not mere presence: an annotation left over from a previous
// hub URL, or one naming a DIFFERENT hive in ?hive=, authorizes against the
// wrong tenant and is worse than absent, because it looks converged.
func terminalAuthzMissing(live, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		if live[k] != v {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// reconcileTerminalAuthzIfDue runs the sweep only if the interval has elapsed,
// so a frequent poller can call it every tick. Mirrors
// reconcilePerHiveEnvIfDue.
func (s *HubServer) reconcileTerminalAuthzIfDue() {
	s.clusterUnreachableMu.Lock()
	due := s.lastTerminalAuthzReconcile.IsZero() ||
		time.Since(s.lastTerminalAuthzReconcile) >= terminalAuthzReconcileInterval
	if due {
		s.lastTerminalAuthzReconcile = time.Now()
	}
	s.clusterUnreachableMu.Unlock()
	if !due {
		return
	}
	s.reconcileTerminalAuthz()
}

// reconcileTerminalAuthz sweeps every hub-managed hosted hive and patches the
// per-hive authorization annotations back onto its hive-terminal Ingress where
// they are absent or point somewhere else.
//
// Idempotent, non-fatal, and bounded. A spoke on the OpenShift-Route lane has no
// Ingress named hive-terminal at all, so the get fails and it is skipped — which
// is correct, not a gap: Routes carry no nginx auth annotations, and that lane's
// per-hive gate is the proxy's own fail-closed allowlist check (HIVE_INGRESS_AUTHZ
// is deliberately unset there).
func (s *HubServer) reconcileTerminalAuthz() {
	patched := 0
	converged := 0
	skipped := 0
	for _, h := range listSaaSHives() {
		if !perHiveEnvSweepEligible(h.Status) {
			skipped++
			continue
		}
		cluster := s.clusterForHive(&h)
		if cluster == nil || s.clusterRecentlyUnreachable(cluster.ID) {
			skipped++
			continue
		}
		want := terminalAuthzAnnotations(h.ID)
		if want == nil {
			s.logger.Warn("terminal authz reconcile: no hub public URL or hive id — NOT patching (an empty auth-url would break the ingress)",
				"hive_id", h.ID)
			skipped++
			continue
		}
		if patched >= terminalAuthzMaxPatchesPerCycle {
			// Deferred, not dropped: the next cycle picks it up.
			break
		}

		ctx, cancel := context.WithTimeout(context.Background(), terminalAuthzKubectlTimeout)
		raw, err := kubectlForClusterContext(ctx, cluster, "-n", hiveHostedNamespacePrefix+h.ID,
			"get", "ingress", "hive-terminal", "-o", "jsonpath={.metadata.annotations}").Output()
		cancel()
		if err != nil {
			// No such Ingress (Route lane), cluster unreachable, or a transient
			// kubectl error. All non-fatal and retried next sweep.
			s.logger.Debug("terminal authz reconcile: could not read hive-terminal ingress annotations",
				"hive_id", h.ID, "cluster", cluster.ID, "err", err)
			skipped++
			continue
		}
		live := map[string]string{}
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			if err := json.Unmarshal([]byte(trimmed), &live); err != nil {
				s.logger.Debug("terminal authz reconcile: unreadable annotations json",
					"hive_id", h.ID, "err", err)
				skipped++
				continue
			}
		}
		missing := terminalAuthzMissing(live, want)
		if missing == nil {
			converged++
			continue
		}

		// A merge patch of metadata.annotations ADDS the missing keys and leaves
		// every other annotation (cert-manager issuer, proxy timeouts, anything
		// an operator added by hand) untouched. Never replace the map wholesale.
		body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": missing}})
		if err != nil {
			skipped++
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), terminalAuthzKubectlTimeout)
		out, err := kubectlForClusterContext(ctx, cluster, "-n", hiveHostedNamespacePrefix+h.ID,
			"patch", "ingress", "hive-terminal", "--type", "merge", "-p", string(body)).CombinedOutput()
		cancel()
		if err != nil {
			s.logger.Warn("terminal authz reconcile: patch failed",
				"hive_id", h.ID, "cluster", cluster.ID, "err", err, "output", strings.TrimSpace(string(out)))
			skipped++
			continue
		}
		patched++
		keys := make([]string, 0, len(missing))
		for k := range missing {
			keys = append(keys, k)
		}
		s.logger.Warn("terminal authz reconcile: repaired MISSING per-hive authorization on hive-terminal ingress (CWE-862, finding C3)",
			"hive_id", h.ID, "cluster", cluster.ID, "annotations", strings.Join(keys, ","))
	}
	if patched > 0 || converged > 0 || skipped > 0 {
		s.logger.Info("terminal authz reconcile sweep complete",
			"patched", patched, "converged", converged, "skipped", skipped)
	}
}
