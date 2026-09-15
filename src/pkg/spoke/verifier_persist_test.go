package spoke

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These tests pin the issue #7121 fix: the trust-on-first-signed flag and the
// rollback-seq floor must survive a process restart, so a restart cannot be
// used to strip signatures (downgrade back to the pre-trust accept path) or to
// replay a previously captured signed response.

// A "restart" is a fresh NewPersistentVerifier on the same state path.
func restart(t *testing.T, mode Mode, path string) *Verifier {
	t.Helper()
	return NewPersistentVerifier(mode, quietLogger(), path)
}

// Restarted enforcing spoke must reject an unsigned response: seenSigned
// persisted, so the signature-strip/downgrade window does not reopen.
func TestPersistedTrustSurvivesRestartDowngrade(t *testing.T) {
	s := newSigner(t)
	path := DefaultStatePath(t.TempDir())

	v1 := restart(t, ModeEnforce, path)
	body, sig := s.signed(t, "hive-a", 10, "creds")
	if got := v1.Verify([]string{s.pubHex}, "hive-a", body, sig, time.Now()); !got.Accepted || !got.Signed {
		t.Fatalf("establishing trust: %+v", got)
	}

	v2 := restart(t, ModeEnforce, path)
	got := v2.Verify([]string{s.pubHex}, "hive-a", body, "", time.Now())
	if got.Accepted || got.Reason != ReasonDowngrade {
		t.Fatalf("unsigned after restart: want rejected %s, got %+v", ReasonDowngrade, got)
	}
}

// Restarted enforcing spoke must reject a replay of an already-accepted seq:
// lastSeq persisted, so the rollback floor does not reset to 0.
func TestPersistedSeqFloorSurvivesRestartReplay(t *testing.T) {
	s := newSigner(t)
	path := DefaultStatePath(t.TempDir())

	oldBody, oldSig := s.signed(t, "hive-a", 5, "old-creds")
	newBody, newSig := s.signed(t, "hive-a", 9, "new-creds")

	v1 := restart(t, ModeEnforce, path)
	if got := v1.Verify([]string{s.pubHex}, "hive-a", oldBody, oldSig, time.Now()); !got.Accepted {
		t.Fatalf("seq 5: %+v", got)
	}
	if got := v1.Verify([]string{s.pubHex}, "hive-a", newBody, newSig, time.Now()); !got.Accepted {
		t.Fatalf("seq 9: %+v", got)
	}

	v2 := restart(t, ModeEnforce, path)
	if got := v2.Verify([]string{s.pubHex}, "hive-a", oldBody, oldSig, time.Now()); got.Accepted || got.Reason != ReasonStaleSeq {
		t.Fatalf("replay of seq 5 after restart: want rejected %s, got %+v", ReasonStaleSeq, got)
	}
	// A genuinely newer seq still advances normally.
	newer, newerSig := s.signed(t, "hive-a", 12, "newer-creds")
	if got := v2.Verify([]string{s.pubHex}, "hive-a", newer, newerSig, time.Now()); !got.Accepted || !got.Signed {
		t.Fatalf("seq 12 after restart: %+v", got)
	}
}

// Corrupt or missing state must FAIL OPEN to fresh-start behaviour — never
// brick the spoke. An unsigned response is then accepted pre-trust, exactly as
// a brand-new spoke would.
func TestCorruptStateFailsOpen(t *testing.T) {
	dir := t.TempDir()
	path := DefaultStatePath(dir)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	v := restart(t, ModeEnforce, path)
	got := v.Verify(nil, "hive-a", []byte(`{"ok":true}`), "", time.Now())
	if !got.Accepted || got.Reason != ReasonUnsignedUntrusted {
		t.Fatalf("corrupt state: want fail-open pre-trust accept, got %+v", got)
	}
}

// An empty state path keeps the pre-fix memory-only behaviour and writes
// nothing to disk.
func TestEmptyStatePathIsMemoryOnly(t *testing.T) {
	s := newSigner(t)
	v := NewPersistentVerifier(ModeEnforce, quietLogger(), "")
	body, sig := s.signed(t, "hive-a", 1, "creds")
	if got := v.Verify([]string{s.pubHex}, "hive-a", body, sig, time.Now()); !got.Accepted {
		t.Fatalf("signed: %+v", got)
	}
}

// The state file is written 0600 and atomically (no lingering .tmp).
func TestStateFilePermissionsAndAtomicity(t *testing.T) {
	s := newSigner(t)
	dir := t.TempDir()
	path := DefaultStatePath(dir)
	v := restart(t, ModeEnforce, path)
	body, sig := s.signed(t, "hive-a", 3, "creds")
	if got := v.Verify([]string{s.pubHex}, "hive-a", body, sig, time.Now()); !got.Accepted {
		t.Fatalf("signed: %+v", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode: want 0600, got %o", perm)
	}
	if _, err := os.Stat(filepath.Join(dir, "heartbeat-sig-state.json.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}
