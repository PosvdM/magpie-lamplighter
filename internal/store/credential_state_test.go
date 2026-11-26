package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The time of the last failure keeps its nanoseconds across a save; state
// saved before failed_at existed has only since, to the second, and its
// last failure counts as the end of that second.
func TestCredentialLastFailure(t *testing.T) {
	failed := time.Date(2026, 11, 21, 2, 3, 4, 567891234, time.UTC)
	f := &StateFile{Path: filepath.Join(t.TempDir(), "state.json")}
	s := &State{Credentials: map[string]*CredentialState{"4": {Reason: "auth", Since: failed.Unix(), FailedAt: failed}}}
	if err := f.Save(s); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := f.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Credentials["4"].LastFailure(); !got.Equal(failed) {
		t.Fatalf("last failure %v", got)
	}
	legacy := &CredentialState{Reason: "auth", Since: failed.Unix()}
	if got := legacy.LastFailure(); !got.Equal(time.Unix(failed.Unix()+1, 0).Add(-time.Nanosecond)) {
		t.Fatalf("legacy last failure %v", got)
	}
	if got := (&CredentialState{}).LastFailure(); !got.IsZero() {
		t.Fatalf("no failure %v", got)
	}
}
