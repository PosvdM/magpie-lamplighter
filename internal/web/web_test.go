package web

import (
	"bytes"
	"strings"
	"testing"
)

func TestWithToken(t *testing.T) {
	page := []byte(`<head><meta name="lamplighter-page" content=""></head><script>var tag = 'meta[name="lamplighter-page"]';</script>`)
	got := string(withToken(page, "abc-_1"))
	want := `<head><meta name="lamplighter-page" content="abc-_1"></head><script>var tag = 'meta[name="lamplighter-page"]';</script>`
	if got != want {
		t.Fatalf("got %s", got)
	}
	if !strings.Contains(string(page), `content=""`) {
		t.Fatal("withToken changed the page it was given")
	}
	if got := string(withToken(page, `"><script>`)); !strings.Contains(got, `content="&#34;&gt;&lt;script&gt;"`) {
		t.Fatalf("token not escaped: %s", got)
	}
	plain := []byte("<head></head>")
	if got := withToken(plain, "abc"); !bytes.Equal(got, plain) {
		t.Fatalf("a page without the tag changed: %s", got)
	}
}

func TestPageWith(t *testing.T) {
	page := Page()
	got := PageWith("token123")
	if !bytes.Contains(page, []byte(pageTokenTag)) {
		if !bytes.Equal(got, page) {
			t.Fatal("a page without the tag must be returned as it is")
		}
		return
	}
	if !bytes.Contains(got, []byte(`<meta name="lamplighter-page" content="token123">`)) || bytes.Contains(got, []byte(pageTokenTag)) {
		t.Fatal("the token was not filled in")
	}
	if !bytes.Contains(Page(), []byte(pageTokenTag)) {
		t.Fatal("PageWith changed Page")
	}
}

// The logo is part of the page, so the page loads nothing from GitHub or a
// mirror of it.
func TestPageInlinesLogo(t *testing.T) {
	page := string(Page())
	if !strings.Contains(page, `<img class="logo" src="data:image/png;base64,`) || strings.Contains(page, "/*LOGO*/") {
		t.Fatal("the logo is not inlined")
	}
	for _, host := range []string{"raw.githubusercontent.com", "ghfast.top"} {
		if strings.Contains(page, host) {
			t.Fatalf("the page refers to %s", host)
		}
	}
	if !bytes.HasPrefix(logoPNG, []byte("\x89PNG")) || len(logoPNG) > 16<<10 {
		t.Fatalf("logo.png is not a small PNG: %d bytes", len(logoPNG))
	}
}
