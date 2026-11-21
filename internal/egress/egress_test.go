package egress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PosvdM/magpie-lamplighter/internal/quota"
)

func TestDirectRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	}))
	defer server.Close()
	resp, err := (&Egress{}).Do(context.Background(), "direct", quota.Request{Method: "GET", URL: server.URL})
	if err != nil || string(resp.Body) != "direct" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
}

func TestAccountProxyIsUsed(t *testing.T) {
	var seen string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.String()
		w.Write([]byte("proxied"))
	}))
	defer proxy.Close()
	resp, err := (&Egress{}).Do(context.Background(), proxy.URL, quota.Request{Method: "GET", URL: "http://upstream.invalid/usage", Header: http.Header{}})
	if err != nil || string(resp.Body) != "proxied" {
		t.Fatalf("resp %+v err %v", resp, err)
	}
	if seen != "http://upstream.invalid/usage" {
		t.Fatalf("proxy saw %q", seen)
	}
}

func TestInvalidProxyFailsClosed(t *testing.T) {
	_, err := (&Egress{}).Do(context.Background(), "ftp://user:secret@bad", quota.Request{Method: "GET", URL: "https://example.com"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid proxy must fail without its password: %v", err)
	}
}
