package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/openai"
)

func TestStreamMapsRequestAndAggregatesToolDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request = %s %s", r.Method, r.URL)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		assertPayload(t, got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"read_page","arguments":"{\"url\":\"https:"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"//example.test\"}"}}]}}]}

data: [DONE]

`))
	}))
	defer server.Close()

	provider := openai.New("test-key", openai.WithBaseURL(server.URL+"/v1"), openai.WithHTTPClient(server.Client()))
	var events []agent.ModelEvent
	if err := provider.Stream(context.Background(), request(), func(event agent.ModelEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	if event, ok := events[0].(agent.TextDelta); !ok || event.Text != "Hel" {
		t.Fatalf("first event = %#v", events[0])
	}
	if event, ok := events[1].(agent.TextDelta); !ok || event.Text != "lo" {
		t.Fatalf("second event = %#v", events[1])
	}
	response, ok := events[2].(agent.ModelResponse)
	if !ok || len(response.Parts) != 2 || response.Parts[0].Text.Text != "Hello" {
		t.Fatalf("response = %#v", events[2])
	}
	call := response.Parts[1].ToolCall
	if call.CallID != "call-1" || call.Name != "read_page" || !reflect.DeepEqual(call.Arguments, map[string]any{"url": "https://example.test"}) || call.ProviderData != nil {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestStreamSynthesizesMissingToolCallID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, wreq *http.Request) {
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	var response agent.ModelResponse
	provider := openai.New("key", openai.WithBaseURL(server.URL))
	if err := provider.Stream(context.Background(), request(), func(event agent.ModelEvent) error {
		if value, ok := event.(agent.ModelResponse); ok {
			response = value
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(response.Parts) != 1 || response.Parts[0].ToolCall.CallID != "openai-2" {
		t.Fatalf("response = %#v", response)
	}
}

func TestStreamErrors(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"rate limit": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"no final": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		},
		"malformed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("data: {not json}\n\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(handler)
			defer server.Close()
			err := openai.New("key", openai.WithBaseURL(server.URL)).Stream(context.Background(), request(), nil)
			if name == "rate limit" {
				var limited *openai.RateLimitError
				if !errors.As(err, &limited) || limited.StatusCode != http.StatusTooManyRequests || limited.RetryAfter != 2*time.Second {
					t.Fatalf("error = %#v", err)
				}
				return
			}
			if !errors.Is(err, openai.ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestStreamHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- openai.New("key", openai.WithBaseURL(server.URL)).Stream(ctx, request(), nil) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func request() agent.ModelRequest {
	return agent.ModelRequest{
		Model:             agent.ModelRef{Provider: "openai", Model: "gpt-test"},
		SystemInstruction: "instruction",
		Messages: []agent.Message{
			{Role: agent.RoleSystem, Parts: []agent.MessagePart{{Text: &agent.TextPart{Text: "system"}}}},
			{Role: agent.RoleUser, Parts: []agent.MessagePart{{Text: &agent.TextPart{Text: "hello"}}, {Image: &agent.ImagePart{Data: []byte("png"), MediaType: "image/png"}}}},
			{Role: agent.RoleAssistant, Parts: []agent.MessagePart{{ToolCall: &agent.ToolCallPart{CallID: "call-1", Name: "read_page", Arguments: map[string]any{"url": "https://example.test"}}}}},
			{Role: agent.RoleTool, Parts: []agent.MessagePart{{ToolResult: &agent.ToolResultPart{CallID: "call-1", Name: "read_page", Result: map[string]any{"text": "page"}}}}},
		},
		Tools:      []agent.Tool{{Name: "read_page", Description: "Read a page.", InputSchema: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`)}},
		Generation: agent.GenerationOptions{Temperature: ptr(0.25), MaxOutputTokens: ptr(123), JSONMode: true},
	}
}

func ptr[T any](value T) *T { return &value }

func assertPayload(t *testing.T, got map[string]any) {
	t.Helper()
	want := map[string]any{
		"model": "gpt-test", "stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "instruction"},
			map[string]any{"role": "system", "content": "system"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,cG5n"}}}},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read_page", "arguments": `{"url":"https://example.test"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": `{"text":"page"}`},
		},
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "read_page", "description": "Read a page.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}, "required": []any{"url"}}}}},
		"temperature": 0.25, "max_tokens": float64(123), "response_format": map[string]any{"type": "json_object"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %#v\nwant %#v", got, want)
	}
}
