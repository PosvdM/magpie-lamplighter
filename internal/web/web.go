// Package web holds the management page. The HTML, CSS and JavaScript are
// kept in separate files for editing and served as one document.
package web

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"html"
	"strings"
	"sync"
)

var (
	//go:embed page.html
	pageHTML string
	//go:embed page.css
	pageCSS string
	//go:embed page.js
	pageJS string
	// logo.png is assets/logo.png at 64 × 64 pixels, twice the size the
	// page shows it at.
	//go:embed logo.png
	logoPNG []byte

	pageOnce  sync.Once
	pageBytes []byte
)

// Page returns the management page with its logo, CSS and JavaScript
// inlined. The logo is a data: URI, so the page loads nothing from the
// network and needs no route of its own.
func Page() []byte {
	pageOnce.Do(func() {
		page := strings.Replace(pageHTML, "/*LOGO*/", "data:image/png;base64,"+base64.StdEncoding.EncodeToString(logoPNG), 1)
		page = strings.Replace(page, "/*CSS*/", pageCSS, 1)
		page = strings.Replace(page, "/*JS*/", pageJS, 1)
		pageBytes = []byte(page)
	})
	return pageBytes
}

// pageTokenTag is the empty tag in page.html that PageWith fills in.
const pageTokenTag = `<meta name="lamplighter-page" content="">`

// PageWith returns the page with token, the internal listener's page
// token, in its lamplighter-page tag; the page sends it back in the
// X-Lamplighter-Page header of its API requests. A page without the tag is
// returned as it is.
func PageWith(token string) []byte {
	return withToken(Page(), token)
}

func withToken(page []byte, token string) []byte {
	tag := []byte(pageTokenTag)
	if !bytes.Contains(page, tag) {
		return page
	}
	filled := `<meta name="lamplighter-page" content="` + html.EscapeString(token) + `">`
	return bytes.Replace(page, tag, []byte(filled), 1)
}
