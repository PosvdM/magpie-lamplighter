package redact

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

func TestText(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl"
	hex := strings.Repeat("0123456789abcdef", 2)
	for _, c := range []struct{ in, want string }{
		{"alice.work@example.com said no", "***rk said no"},
		{`<"a.b"@x.io>`, `<"a.b"@x.io>`},
		{"user a.b@x.io.", "user ***.b."},
		{"账号 carol@corp.example：失败", "账号 ***ol：失败"},
		{"GET https://u:pw@vendor.example/v1/usage?user=a%40b.c&sig=x#frag failed", "GET https://vendor.example/v1/usage failed"},
		{`Get "http://127.0.0.1:3425/v1/magpie/quotas/refresh?provider=claude&user=b%40example.com": EOF`, `Get "http://127.0.0.1:3425/v1/magpie/quotas/refresh": EOF`},
		{"see https://forms.gle/hGzM9MEUv2azZsrb9", "see https://forms.gle/hGzM9MEUv2azZsrb9"},
		{"Authorization: Bearer abc.DEF-ghi_jkl~", "Authorization: Bearer …"},
		{"basic dXNlcjpwYXNz==", "basic …"},
		{"token " + jwt + " expired", "token … expired"},
		{"access_token=abc123&x=1", "access_token=…&x=1"},
		{`{"refresh_token": "1//0gShort", "n": 1}`, `{"refresh_token": "…", "n": 1}`},
		{"api-key: k-1", "api-key: …"},
		{"key " + hex, "key …"},
		{"sk-ant-oat01-" + strings.Repeat("x", 40), "…"},
		{"ya29." + strings.Repeat("A1b2", 10), "ya29.…"},
		// Values that are no secrets stay.
		{"codex/1a2b3c4d5e6f7a8b at 2026-11-21T07:00:00Z", "codex/1a2b3c4d5e6f7a8b at 2026-11-21T07:00:00Z"},
		{"model claude-sonnet-4@20250514 not found", "model claude-sonnet-4@20250514 not found"},
		{"max_tokens: 1024, input_tokens=5", "max_tokens: 1024, input_tokens=5"},
		{"POST /v1/magpie/quotas/refresh: request timed out", "POST /v1/magpie/quotas/refresh: request timed out"},
		{"X-Magpie-Account: bob@example.com rests until 2026-11-21T08:00:00Z (limit)", "X-Magpie-Account: ***ob rests until 2026-11-21T08:00:00Z (limit)"},
		{"", ""},
	} {
		got := Text(c.in)
		if got != c.want {
			t.Errorf("Text(%q) = %q, want %q", c.in, got, c.want)
		}
		if again := Text(got); again != got {
			t.Errorf("Text(%q) = %q, not %q", got, again, got)
		}
	}
}

// A redacted JSON error body still parses, with its fields where they
// were.
func TestTextKeepsJSON(t *testing.T) {
	body := `{"error":{"message":"a@b.co: Bearer ` + strings.Repeat("Q", 40) + ` at https://x.io/p?user=a%40b.co","code":"invalid_grant","param":{"access_token":"t0k"}}}`
	var v map[string]any
	if err := json.Unmarshal([]byte(Text(body)), &v); err != nil {
		t.Fatalf("%s: %v", Text(body), err)
	}
	e := v["error"].(map[string]any)
	if e["code"] != "invalid_grant" || e["message"] != "***a: Bearer … at https://x.io/p" || e["param"].(map[string]any)["access_token"] != "…" {
		t.Fatalf("redacted %v", v)
	}
}

func TestParams(t *testing.T) {
	if Params(nil) != nil {
		t.Fatal("nil params")
	}
	in := map[string]string{"error": "x@y.io", "model": "gpt-6"}
	out := Params(in)
	if out["error"] != "***x" || out["model"] != "gpt-6" || in["error"] != "x@y.io" {
		t.Fatalf("in %v out %v", in, out)
	}
}

func TestWriter(t *testing.T) {
	var buf bytes.Buffer
	l := log.New(Writer(&buf), "lamplighter: ", 0)
	l.Printf("open https://h.io/x?token=1 for carol@corp.example")
	if got := buf.String(); got != "lamplighter: open https://h.io/x for ***ol\n" {
		t.Fatalf("log %q", got)
	}
}
