package engine

import (
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/config"
	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// Magpie's label of an account the user has not renamed is its user, which
// for Claude and Codex is the email and the workspace. Such a label is no
// name, or the page and MCP would show the full email.
func TestAccountNoteIgnoresMagpieDefaultLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		account host.Account
		want    string
	}{
		{"empty label", config.Config{}, host.Account{Pin: "claude/1", Email: "claude1@example.com"}, ""},
		{"email label", config.Config{}, host.Account{Pin: "agy-cli/1", Label: "agy@example.com", Email: "agy@example.com", User: "agy@example.com"}, ""},
		{"user label", config.Config{}, host.Account{Pin: "claude/1", Label: "claude1@example.com · Example", Email: "claude1@example.com", User: "claude1@example.com · Example"}, ""},
		{"user label, other case and spaces", config.Config{}, host.Account{Pin: "codex/1", Label: " Team6+eg@Example.com · Team ", Email: "team6+eg@example.com", User: "team6+eg@example.com · Team"}, ""},
		{"renamed in Magpie", config.Config{}, host.Account{Pin: "claude/1", Label: " Work ", Email: "claude1@example.com", User: "claude1@example.com · Example"}, "Work"},
		{"account_names first", config.Config{AccountNames: map[string]string{"claude/1": "Mine"}}, host.Account{Pin: "claude/1", Label: "Work", User: "claude1@example.com"}, "Mine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountNote(tc.cfg, tc.account); got != tc.want {
				t.Fatalf("accountNote = %q, want %q", got, tc.want)
			}
		})
	}
	// The masked MCP label of such an account comes from its email.
	label := accountLabels([]AccountStatus{{Provider: "claude", Email: "claude1@example.com", Note: accountNote(config.Config{}, host.Account{Label: "claude1@example.com · Example", Email: "claude1@example.com", User: "claude1@example.com · Example"})}})
	if label[0] != "***e1" {
		t.Fatalf("label = %q, want ***e1", label[0])
	}
}
