package rotation

import (
	"os"
	"path/filepath"
	"testing"
)

// The window-label helpers band provider-stated windows into the shared kinds
// the contributor guard evaluates (kubestellar/hive#6951, #6965, #6986). Every
// branch matters: an unmapped label must yield zero ("the provider did not
// state one"), never a guessed duration.

func TestAgyWindowDurationMins(t *testing.T) {
	cases := map[string]int{
		"session":   60,
		"five_hour": 300,
		"short":     300,
		"daily":     1440,
		"weekly":    10080,
		"seven_day": 10080,
		"":          0,
		"monthly":   0,
		"bogus":     0,
	}
	for window, want := range cases {
		if got := agyWindowDurationMins(window); got != want {
			t.Errorf("agyWindowDurationMins(%q) = %d, want %d", window, got, want)
		}
	}
}

func TestClaudeWindowDurationMins(t *testing.T) {
	cases := map[string]int{
		"session":       300,
		"five_hour":     300,
		"weekly":        10080,
		"weekly_all":    10080,
		"weekly_scoped": 10080,
		"seven_day":     10080,
		"":              0,
		"monthly":       0,
	}
	for kind, want := range cases {
		if got := claudeWindowDurationMins(kind); got != want {
			t.Errorf("claudeWindowDurationMins(%q) = %d, want %d", kind, got, want)
		}
	}
}

func TestCodexWindowKind(t *testing.T) {
	cases := []struct {
		mins int
		want string
	}{
		{-5, "unknown"},
		{0, "unknown"},
		{60, "session"},
		{300, "five_hour"},
		{1440, "daily"},
		{10080, "weekly"},
		{43200, "window_43200m"},
	}
	for _, c := range cases {
		if got := codexWindowKind(c.mins); got != c.want {
			t.Errorf("codexWindowKind(%d) = %q, want %q", c.mins, got, c.want)
		}
	}
}

// publishContributorReadingAtomic must surface filesystem errors rather than
// silently dropping a reading the relay would otherwise trust as fresh.
func TestPublishContributorReadingAtomicErrors(t *testing.T) {
	reading := ContributorReading{State: "available", Limits: []ContributorLimitWindow{}}

	t.Run("mkdir blocked by file", func(t *testing.T) {
		base := t.TempDir()
		blocker := filepath.Join(base, "notadir")
		if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(blocker, "pool.reading.json")
		if err := publishContributorReadingAtomic(path, reading); err == nil {
			t.Fatal("expected error when parent path is a file, got nil")
		}
	})

	t.Run("success writes readable file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "pool.reading.json")
		if err := publishContributorReadingAtomic(path, reading); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat: %v", err)
		}
		leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*tmp*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(leftovers) != 0 {
			t.Fatalf("temp files left behind: %v", leftovers)
		}
	})
}
