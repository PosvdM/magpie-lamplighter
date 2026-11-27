package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The time a credential problem was last found keeps its nanoseconds
// across a save.
func TestCredentialFailedAt(t *testing.T) {
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
	if got := loaded.Credentials["4"].FailedAt; !got.Equal(failed) {
		t.Fatalf("failed at %v", got)
	}
}
