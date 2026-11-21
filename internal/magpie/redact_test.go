package magpie

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/host"
)

// Log writes every line redacted, debug lines included.
func TestLogIsRedacted(t *testing.T) {
	t.Setenv("LAMPLIGHTER_DEBUG", "1")
	var out bytes.Buffer
	h := &Host{Logger: log.New(&out, "", 0)}
	secret := "ya29." + strings.Repeat("Zq9", 12)
	for _, level := range []string{"debug", "warn"} {
		h.Log(level, "carol.secret@corp.example: GET https://vendor.example/v1?user=carol%40corp.example Bearer "+secret, nil)
	}
	for _, part := range []string{"carol.secret", "corp.example", "user=", "Zq9Zq9"} {
		if strings.Contains(out.String(), part) {
			t.Fatalf("log %q shows %q", out.String(), part)
		}
	}
	if !strings.Contains(out.String(), "[debug] ***et: GET https://vendor.example/v1 Bearer …") {
		t.Fatalf("log = %q", out.String())
	}
}

// The errors of an agy-cli login name no value of the files: the home
// that logins.json gives is the key of plugin-auth.json, any text.
func TestAgyErrorsLeaveOutTheHome(t *testing.T) {
	const home = "carol.secret@corp.example"
	logins := strings.ReplaceAll(loginsFile("", "{}"), "agy-cli#k3x9a2", home)
	for name, auth := range map[string]string{
		"no entry":        `{}`,
		"not an object":   `{"` + home + `":[]}`,
		"not api":         `{"` + home + `":{"type":"oauth","key":"agy"}}`,
		"bad agyHome":     `{"` + home + `":{"type":"api","metadata":{"agyHome":"../../etc"}}}`,
		"key not agyHome": `{"` + home + `":{"type":"api","key":"agy","metadata":{"agyHome":"0123456789abcdef"}}}`,
	} {
		h := &Host{ConfigDir: t.TempDir()}
		writeFiles(t, h.ConfigDir, map[string]string{"logins.json": logins, "plugin-auth.json": auth})
		_, err := h.Credential(context.Background(), agyAccount)
		if err == nil || strings.Contains(err.Error(), "carol") || strings.Contains(err.Error(), "oauth") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A window that does not convert is named only when its name is one
// Lamplighter knows: the name comes from Magpie's card.
func TestWindowNameInErrors(t *testing.T) {
	_, err := keptClaude([]keptWindow{{Name: "carol.secret@corp.example"}})
	if !errors.Is(err, host.ErrFormat) || strings.Contains(err.Error(), "carol") {
		t.Fatalf("unknown name: %v", err)
	}
	_, err = keptCodex([]keptWindow{{Name: "5 Hours"}})
	if !errors.Is(err, host.ErrFormat) || !strings.Contains(err.Error(), `window "5 hours"`) {
		t.Fatalf("known name: %v", err)
	}
}
