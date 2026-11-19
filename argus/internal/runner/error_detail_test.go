package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/gemini"
	"github.com/ace-foundry/argus-testing/argus/internal/openai"
)

func TestErrorDetailIncludesGeminiStatusAndBody(t *testing.T) {
	body := "{\n  \"error\": {\n    \"code\": 404,\n    \"message\": \"This model models/gemini-2.5-flash is no longer available to new users.\"\n  }\n}"
	err := fmt.Errorf("validator: %w", &gemini.HTTPError{StatusCode: 404, Body: body})

	got := errorDetail(err)

	for _, want := range []string{"validator: Gemini API returned HTTP 404", "status=404", `"message": "This model models/gemini-2.5-flash is no longer available to new users."`} {
		if !strings.Contains(got, want) {
			t.Fatalf("errorDetail() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("errorDetail() should be a single log line, got %q", got)
	}
	if publicError(err) != "Gemini provider request failed" {
		t.Fatalf("publicError() must stay generic, got %q", publicError(err))
	}
}

func TestErrorDetailIncludesRateLimitRetryAfter(t *testing.T) {
	err := &openai.RateLimitError{StatusCode: 429, RetryAfter: 30 * time.Second, Body: `{"error":{"message":"quota exceeded"}}`}

	got := errorDetail(err)

	for _, want := range []string{"status=429", "retry_after=30s", "quota exceeded"} {
		if !strings.Contains(got, want) {
			t.Fatalf("errorDetail() = %q, missing %q", got, want)
		}
	}
}

func TestErrorDetailTruncatesLargeBodies(t *testing.T) {
	err := &gemini.HTTPError{StatusCode: 500, Body: strings.Repeat("x", maxLoggedBody*3)}

	got := errorDetail(err)

	if !strings.HasSuffix(got, "…(truncated)") || len(got) > maxLoggedBody+200 {
		t.Fatalf("errorDetail() was not truncated: len=%d", len(got))
	}
}

func TestErrorDetailFallsBackToErrorChain(t *testing.T) {
	err := fmt.Errorf("strategist: %w", errors.New("malformed handoff"))

	if got := errorDetail(err); got != "strategist: malformed handoff" {
		t.Fatalf("errorDetail() = %q", got)
	}
}
