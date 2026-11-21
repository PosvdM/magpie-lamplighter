package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The error answers of the API leave out emails, URL parameters and
// tokens: the refresh and the other actions, the tool route and every
// other error.
func TestErrorAnswersAreRedacted(t *testing.T) {
	const leaky = "carol.secret@corp.example at https://vendor.example/v1?user=carol%40corp.example: Bearer ya29.A0b1C2d3E4f5G6h7I8j9K0l1M2n3O4p5Q6r7S8t9"
	s, _ := newServer(t)
	err := errors.New(leaky)
	for name, write := range map[string]func(http.ResponseWriter){
		"action":     func(w http.ResponseWriter) { s.action(w, err) },
		"tool error": func(w http.ResponseWriter) { writeToolError(w, err) },
		"error":      func(w http.ResponseWriter) { writeError(w, http.StatusInternalServerError, err) },
	} {
		rec := httptest.NewRecorder()
		write(rec)
		body := rec.Body.String()
		if !strings.Contains(body, `"error":"`) {
			t.Fatalf("%s: %s", name, body)
		}
		for _, part := range []string{"carol.secret", "corp.example", "user=", "A0b1C2d3E4f5G6h7I8j9"} {
			if strings.Contains(body, part) {
				t.Errorf("%s: %s shows %q", name, body, part)
			}
		}
	}
}
