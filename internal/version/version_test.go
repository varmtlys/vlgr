package version

import "testing"

func TestShort(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	defer func() { Version = old }()
	if got := Short(); got != "v1.2.3" {
		t.Errorf("Short() = %q, want %q", got, "v1.2.3")
	}
}

func TestString_Format(t *testing.T) {
	oldV, oldC, oldD := Version, GitCommit, BuildDate
	Version = "v1.0.0"
	GitCommit = "abc1234"
	BuildDate = "2026-07-09T12:00:00Z"
	defer func() { Version, GitCommit, BuildDate = oldV, oldC, oldD }()
	want := "v1.0.0 (commit: abc1234, built: 2026-07-09T12:00:00Z)"
	if got := String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestDefaults(t *testing.T) {
	if Version == "" {
		t.Error("Version should have a default value")
	}
	if GitCommit == "" {
		t.Error("GitCommit should have a default value")
	}
	if BuildDate == "" {
		t.Error("BuildDate should have a default value")
	}
}
