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
