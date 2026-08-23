// Package openai implements the OpenAI-compatible chat completions streaming API without an SDK.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
)

const defaultBaseURL = "https://api.openai.com/v1"

var (
	ErrInvalidResponse = errors.New("invalid OpenAI-compatible response")
	ErrInvalidBaseURL  = errors.New("invalid OpenAI-compatible base URL")
)

type RateLimitError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string { return "OpenAI-compatible API rate limit exceeded" }

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("OpenAI-compatible API returned HTTP %d", e.StatusCode)
}

type Option func(*Provider)

func WithBaseURL(baseURL string) Option {
	return func(provider *Provider) { provider.baseURL = strings.TrimSpace(baseURL) }
}

func WithHTTPClient(client *http.Client) Option {
	return func(provider *Provider) { provider.client = client }
}

type Provider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func New(apiKey string, options ...Option) *Provider {
	provider := &Provider{apiKey: apiKey, baseURL: defaultBaseURL, client: http.DefaultClient}
	for _, option := range options {
		if option != nil {
			option(provider)
		}
	}
	if provider.client == nil {
		provider.client = http.DefaultClient
	}
	return provider
}

func (p *Provider) Stream(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelEvent) error) error {
	payload, err := payloadFor(request)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal OpenAI-compatible request: %w", err)
	}
	endpoint, err := p.endpointURL()
	if err != nil {
		return err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create OpenAI-compatible request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")

	response, err := p.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send OpenAI-compatible request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{StatusCode: response.StatusCode, RetryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now())}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		limitedBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return &HTTPError{StatusCode: response.StatusCode, Body: string(limitedBody)}
	}
	return streamSSE(ctx, response.Body, emit)
}

func (p *Provider) endpointURL() (string, error) {
	base, err := url.Parse(p.baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", ErrInvalidBaseURL
	}
	base.Path = path.Join("/", base.Path, "chat", "completions")
	base.RawPath = ""
	return base.String(), nil
}

func payloadFor(request agent.ModelRequest) (map[string]any, error) {
	messages := make([]any, 0, len(request.Messages)+1)
	if request.SystemInstruction != "" {
		messages = append(messages, map[string]any{"role": "system", "content": request.SystemInstruction})
	}
	for _, message := range request.Messages {
		mapped, err := mapMessage(message)
		if err != nil {
			return nil, err
		}
		messages = append(messages, mapped...)
	}
	payload := map[string]any{"model": request.Model.Model, "messages": messages, "stream": true}
	if len(request.Tools) > 0 {
		tools := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			parameters := any(map[string]any{})
			if len(tool.InputSchema) > 0 {
				if err := json.Unmarshal(tool.InputSchema, &parameters); err != nil {
					return nil, fmt.Errorf("tool %q input schema: %w", tool.Name, err)
				}
				if _, ok := parameters.(map[string]any); !ok {
					return nil, fmt.Errorf("tool %q input schema must be a JSON object", tool.Name)
				}
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": parameters}})
		}
		payload["tools"] = tools
	}
	if request.Generation.Temperature != nil {
		payload["temperature"] = *request.Generation.Temperature
	}
	if request.Generation.MaxOutputTokens != nil {
		payload["max_tokens"] = *request.Generation.MaxOutputTokens
	}
	if request.Generation.JSONMode {
		payload["response_format"] = map[string]any{"type": "json_object"}
	}
	return payload, nil
}

func mapMessage(message agent.Message) ([]any, error) {
	role := string(message.Role)
	switch message.Role {
	case agent.RoleSystem, agent.RoleUser:
		content, err := messageContent(message.Parts)
		if err != nil {
			return nil, err
		}
		return []any{map[string]any{"role": role, "content": content}}, nil
	case agent.RoleAssistant:
		return mapAssistantMessage(message)
	case agent.RoleTool:
		return mapToolMessages(message)
	default:
		return nil, fmt.Errorf("unsupported OpenAI-compatible message role %q", message.Role)
	}
}

func messageContent(parts []agent.MessagePart) (any, error) {
	if len(parts) == 1 && parts[0].Text != nil {
		return parts[0].Text.Text, nil
	}
	content := make([]any, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != nil:
			content = append(content, map[string]any{"type": "text", "text": part.Text.Text})
		case part.Image != nil:
			if part.Image.MediaType == "" || strings.ContainsAny(part.Image.MediaType, "\r\n") {
				return nil, errors.New("invalid image media type")
			}
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + part.Image.MediaType + ";base64," + base64.StdEncoding.EncodeToString(part.Image.Data)}})
		default:
			return nil, errors.New("unsupported OpenAI-compatible message part")
		}
	}
	if len(content) == 0 {
		return nil, errors.New("empty message parts")
	}
	return content, nil
}

func mapAssistantMessage(message agent.Message) ([]any, error) {
	textParts := make([]agent.MessagePart, 0, len(message.Parts))
	toolCalls := make([]any, 0)
	for _, part := range message.Parts {
		switch {
		case part.Text != nil || part.Image != nil:
			textParts = append(textParts, part)
		case part.ToolCall != nil:
			if part.ToolCall.Name == "" {
				return nil, errors.New("tool call has no name")
			}
			arguments, err := json.Marshal(nonNilArguments(part.ToolCall.Arguments))
			if err != nil {
				return nil, fmt.Errorf("marshal tool call arguments: %w", err)
			}
			callID := part.ToolCall.CallID
			if callID == "" {
				callID = fmt.Sprintf("openai-%d", len(toolCalls)+1)
			}
			toolCalls = append(toolCalls, map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": part.ToolCall.Name, "arguments": string(arguments)}})
		default:
			return nil, errors.New("unsupported assistant message part")
		}
	}
	if len(textParts) == 0 && len(toolCalls) == 0 {
		return nil, errors.New("empty message parts")
	}
	mapped := map[string]any{"role": "assistant"}
	if len(textParts) > 0 {
		content, err := messageContent(textParts)
		if err != nil {
			return nil, err
		}
		mapped["content"] = content
	}
	if len(toolCalls) > 0 {
		mapped["tool_calls"] = toolCalls
	}
	return []any{mapped}, nil
}

func mapToolMessages(message agent.Message) ([]any, error) {
	mapped := make([]any, 0, len(message.Parts))
	for _, part := range message.Parts {
		if part.ToolResult == nil || part.ToolResult.CallID == "" {
			return nil, errors.New("tool message must contain a tool result with a call ID")
		}
		content, err := json.Marshal(part.ToolResult.Result)
		if err != nil {
			return nil, fmt.Errorf("marshal tool result: %w", err)
		}
		mapped = append(mapped, map[string]any{"role": "tool", "tool_call_id": part.ToolResult.CallID, "content": string(content)})
	}
	if len(mapped) == 0 {
		return nil, errors.New("empty message parts")
	}
	return mapped, nil
}

func nonNilArguments(arguments map[string]any) map[string]any {
	if arguments == nil {
		return map[string]any{}
	}
	return arguments
}

func streamSSE(ctx context.Context, body io.Reader, emit func(agent.ModelEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var dataLines []string
	var text strings.Builder
	toolCalls := map[int]*toolCallAccumulator{}
	sawResponse := false
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			return nil
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
		}
		if len(chunk.Choices) > 0 {
			sawResponse = true
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if err := emitModelEvent(emit, agent.TextDelta{Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index < 0 {
					return fmt.Errorf("%w: negative tool call index", ErrInvalidResponse)
				}
				call := toolCalls[delta.Index]
				if call == nil {
					call = &toolCallAccumulator{callID: fmt.Sprintf("openai-%d", delta.Index+1)}
					toolCalls[delta.Index] = call
				}
				if delta.ID != "" {
					call.callID = delta.ID
				}
				if delta.Function.Name != "" {
					call.name = delta.Function.Name
				}
				call.arguments.WriteString(delta.Function.Arguments)
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			return fmt.Errorf("%w: malformed SSE line", ErrInvalidResponse)
		}
		if strings.HasPrefix(value, " ") {
			value = strings.TrimPrefix(value, " ")
		}
		if field == "data" {
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read OpenAI-compatible stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if !sawResponse {
		return fmt.Errorf("%w: stream had no final response", ErrInvalidResponse)
	}
	parts := make([]agent.ResponsePart, 0, 1+len(toolCalls))
	if text.Len() > 0 {
		parts = append(parts, agent.ResponsePart{Text: &agent.TextPart{Text: text.String()}})
	}
	indices := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		call := toolCalls[index]
		if call.name == "" {
			return fmt.Errorf("%w: function call has no name", ErrInvalidResponse)
		}
		arguments, err := decodeArguments(call.arguments.String())
		if err != nil {
			return err
		}
		parts = append(parts, agent.ResponsePart{ToolCall: &agent.ToolCallPart{CallID: call.callID, Name: call.name, Arguments: arguments}})
	}
	return emitModelEvent(emit, agent.ModelResponse{Parts: parts})
}

type toolCallAccumulator struct {
	callID    string
	name      string
	arguments strings.Builder
}

func decodeArguments(arguments string) (map[string]any, error) {
	if arguments == "" {
		return map[string]any{}, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(arguments), &decoded); err != nil {
		return nil, fmt.Errorf("%w: function arguments: %v", ErrInvalidResponse, err)
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: function arguments must be a JSON object", ErrInvalidResponse)
	}
	return object, nil
}

func emitModelEvent(emit func(agent.ModelEvent) error, event agent.ModelEvent) error {
	if emit == nil {
		return nil
	}
	return emit(event)
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return when.Sub(now)
	}
	return 0
}
