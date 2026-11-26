package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// googleWords are what a direct request to Google, or a read of agy's
// sign-in, needs: Google's API hosts, Cloud Code's internal methods, agy's
// OAuth client and token file, and the Antigravity Hub's update manifest
// and User-Agent. Lamplighter reads Antigravity only from Magpie, whose
// agy-cli plugin runs the official agy CLI.
var googleWords = regexp.MustCompile(`(?i)googleapis|v1internal|retrieveUserQuotaSummary|loadCodeAssist|fetchAvailableModels|cloudcode-pa|antigravity-hub|antigravity/hub|apps\.googleusercontent|GOCSPX|antigravity-oauth-token|plugin-auth\.json|magpie-agy-bridge`)

// No source shipped with Lamplighter names a Google endpoint or agy's
// credentials. Tests may quote Google's error bodies, which name
// googleapis.com types.
func TestSourcesSendNothingToGoogle(t *testing.T) {
	var checked int
	for _, root := range []string{"main.go", "internal", "npm", "scripts"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "test" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || strings.HasSuffix(name, "_test.cjs") {
				return nil
			}
			switch filepath.Ext(name) {
			case ".go", ".js", ".mjs", ".cjs", ".html", ".css", ".sh":
			default:
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			checked++
			for i, line := range strings.Split(string(b), "\n") {
				if m := googleWords.FindString(line); m != "" {
					t.Errorf("%s:%d names %q", path, i+1, m)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 20 {
		t.Fatalf("checked only %d files", checked)
	}
}
