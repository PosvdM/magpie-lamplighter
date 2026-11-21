package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
)

// pageHeader carries the page token in the page's API requests.
const pageHeader = "X-Lamplighter-Page"

// newPageToken returns a random page token: 32 bytes, base64url without
// padding.
func newPageToken() string {
	b := make([]byte, 32)
	rand.Read(b) // never fails; see crypto/rand.Read
	return base64.RawURLEncoding.EncodeToString(b)
}

// guardInternal wraps h, the routes of the internal listener, with its
// Host check, its check of browser requests and CORS.
//
// The internal listener takes no key. A site that rebinds its own name to
// 127.0.0.1 would be same-origin with it in the browser, so a Host that is
// not a loopback address is refused with 403. Magpie's page service sends
// Host 127.0.0.1:<port> (servePageService in Magpie's
// internal/gui/pageservice.go), the desktop window opens the page at
// 127.0.0.1, and the plugin host calls the tools there.
//
// Any page in a browser can send requests here, and a request runs its
// route whether or not the page can read the response. An /api/ request
// that carries Origin or Sec-Fetch-Site comes from a browser and runs only
// when it comes from the plugin page: see pageRequest. Other requests, such
// as the tool calls of the plugin host, carry neither header and pass.
//
// The plugin page runs in a sandboxed frame, an opaque origin whose
// requests carry Origin: null. The CORS preflight carries no page token
// and is answered for Origin: null without one; it runs no route. The
// responses to the page's requests carry Access-Control-Allow-Origin: null.
func guardInternal(h http.Handler, token string) http.Handler {
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, errors.New("Host 必须是回环地址"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method == http.MethodOptions && nullOrigin(r) {
				header := w.Header()
				header.Set("Access-Control-Allow-Origin", "null")
				header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH")
				header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, "+pageHeader)
				header.Set("Access-Control-Max-Age", "600")
				header.Add("Vary", "Origin")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if fromBrowser(r) {
				actual := sha256.Sum256([]byte(r.Header.Get(pageHeader)))
				if token == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 || !pageOrigin(r) {
					writeError(w, http.StatusForbidden, errors.New("请求不是来自 Lamplighter 页面"))
					return
				}
				if nullOrigin(r) {
					w.Header().Set("Access-Control-Allow-Origin", "null")
					w.Header().Add("Vary", "Origin")
				}
			}
		}
		h.ServeHTTP(w, r)
	})
}

// fromBrowser reports whether r carries a header that browsers add to the
// requests of a page: Origin or Sec-Fetch-Site. A page cannot remove them.
func fromBrowser(r *http.Request) bool {
	return len(r.Header.Values("Origin")) > 0 || len(r.Header.Values("Sec-Fetch-Site")) > 0
}

// pageOrigin reports whether a browser request comes from where the
// plugin page runs: the sandboxed frame (Origin: null) or the internal
// listener itself (its own origin, or Sec-Fetch-Site: same-origin on a
// request without Origin). Host has passed loopbackHost.
func pageOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	switch len(origins) {
	case 0:
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	case 1:
		return origins[0] == "null" || strings.EqualFold(origins[0], "http://"+r.Host)
	default:
		return false
	}
}

// nullOrigin reports whether r carries the single header Origin: null.
func nullOrigin(r *http.Request) bool {
	origins := r.Header.Values("Origin")
	return len(origins) == 1 && origins[0] == "null"
}

// loopbackHost reports whether host, a Host header with or without a port,
// names localhost or a loopback IP address.
func loopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
