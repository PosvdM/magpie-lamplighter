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
// Host check and CORS.
//
// The internal listener takes no key. A site that rebinds its own name to
// 127.0.0.1 would be same-origin with it in the browser, so a Host that is
// not a loopback address is refused with 403. Magpie's page service sends
// Host 127.0.0.1:<port> (servePageService in Magpie's
// internal/gui/pageservice.go), the desktop window opens the page at
// 127.0.0.1, and the plugin host calls the tools there.
//
// The plugin page runs in a sandboxed frame, an opaque origin whose
// requests carry Origin: null. Any site can make such a frame, so an API
// response is readable by it (Access-Control-Allow-Origin: null) only when
// the request carries the page token, which only the page served here
// holds. Other requests are served without CORS headers.
func guardInternal(h http.Handler, token string) http.Handler {
	expected := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			writeError(w, http.StatusForbidden, errors.New("Host 必须是回环地址"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") && nullOrigin(r) {
			if r.Method == http.MethodOptions {
				header := w.Header()
				header.Set("Access-Control-Allow-Origin", "null")
				header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH")
				header.Set("Access-Control-Allow-Headers", "Content-Type, Authorization, "+pageHeader)
				header.Set("Access-Control-Max-Age", "600")
				header.Add("Vary", "Origin")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			actual := sha256.Sum256([]byte(r.Header.Get(pageHeader)))
			if token != "" && subtle.ConstantTimeCompare(expected[:], actual[:]) == 1 {
				w.Header().Set("Access-Control-Allow-Origin", "null")
				w.Header().Add("Vary", "Origin")
			}
		}
		h.ServeHTTP(w, r)
	})
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
