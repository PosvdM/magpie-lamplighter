package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// googleWordsFile lists what a request to Google needs, which no source
// may name; scripts/check-dist.mjs checks the built programs with it.
// Lamplighter reads Antigravity only from Magpie, whose agy-cli plugin
// runs the official agy CLI, and sends Google nothing.
const googleWordsFile = "testdata/google-words.txt"

// googleWords reads googleWordsFile as one case-insensitive pattern.
func googleWords(t *testing.T) *regexp.Regexp {
	t.Helper()
	b, err := os.ReadFile(googleWordsFile)
	if err != nil {
		t.Fatal(err)
	}
	var words []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			words = append(words, line)
		}
	}
	if len(words) < 10 {
		t.Fatalf("%s lists %d words", googleWordsFile, len(words))
	}
	return regexp.MustCompile(`(?i)` + strings.Join(words, "|"))
}

// agyFileWords name agy's sign-in files. Only internal/magpie/agy.go reads
// them, to confirm whose sign-in ignition will use; their tokens stay in
// that file.
var agyFileWords = regexp.MustCompile(`antigravity-oauth-token|plugin-auth\.json|magpie-agy-bridge`)

// agyFiles is the one source that may name agyFileWords.
const agyFiles = "internal/magpie/agy.go"

// No source shipped with Lamplighter names a Google endpoint
// (googleWordsFile), and only agyFiles names agy's sign-in files. Tests
// may quote Google's error bodies, which name Google's API types.
func TestSourcesSendNothingToGoogle(t *testing.T) {
	google := googleWords(t)
	var checked int
	agyRead := false
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
			own := filepath.ToSlash(path) == agyFiles
			for i, line := range strings.Split(string(b), "\n") {
				if m := google.FindString(line); m != "" {
					t.Errorf("%s:%d names %q", path, i+1, m)
				}
				if m := agyFileWords.FindString(line); m != "" {
					if own {
						agyRead = true
					} else {
						t.Errorf("%s:%d names %q, which only %s may", path, i+1, m, agyFiles)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if checked < 20 || !agyRead {
		t.Fatalf("checked only %d files, %s read: %v", checked, agyFiles, agyRead)
	}
}
