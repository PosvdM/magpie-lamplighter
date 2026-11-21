package notify

import (
	"strings"
	"testing"
	"time"
)

// The error summaries and appeal links of the notifications leave out
// emails, URL parameters and tokens; a plain appeal link stays as it is.
func TestNotificationSummariesAreRedacted(t *testing.T) {
	const leaky = "carol.secret@corp.example at https://vendor.example/v1?user=carol%40corp.example: Bearer ya29.A0b1C2d3E4f5G6h7I8j9K0l1M2n3O4p5Q6r7S8t9"
	appeal := "https://forms.gle/hGzM9MEUv2azZsrb9"
	msgs := []Message{CircuitMessage("zh", "Claude#me", time.Now(), leaky, time.UTC)}
	for _, kind := range []string{KindCredential, KindSuspended, KindRateLimited} {
		msgs = append(msgs, CredentialMessage("en", "Claude#me", Problem{Kind: kind, Status: "403", Summary: leaky, AppealURL: appeal}, time.UTC))
	}
	// An appeal link with a query, as a provider's error may give one.
	odd := CredentialMessage("en", "Claude#me", Problem{Kind: KindSuspended, AppealURL: appeal + "?user=carol%40corp.example"}, time.UTC)
	if odd.JumpURL != appeal || !strings.Contains(odd.Body, appeal) {
		t.Errorf("appeal link with a query: %+v", odd)
	}
	msgs = append(msgs, odd)
	for _, msg := range msgs {
		for _, part := range []string{"carol.secret", "corp.example", "user=", "A0b1C2d3E4f5G6h7I8j9"} {
			if strings.Contains(msg.Title+msg.Body, part) {
				t.Errorf("%s: %q shows %q", msg.Kind, msg.Body, part)
			}
		}
		if msg.Kind == KindSuspended && (!strings.Contains(msg.Body, appeal) || msg.JumpURL != appeal) {
			t.Errorf("appeal link: %+v", msg)
		}
	}
}
