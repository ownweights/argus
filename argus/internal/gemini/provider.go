// Package gemini implements Gemini's streaming REST API without the Gemini SDK.
package gemini

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
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"

var ErrInvalidResponse = errors.New("invalid Gemini response")
var ErrInvalidGroundingRequest = errors.New("invalid Gemini grounding request")

type GroundingRequest struct {
	Model       string
	Description string
	Image       []byte
	Width       int
	Height      int
	Limit       int
}

type VisualMatch struct {
	X           int
	Y           int
	Confidence  float64
	Description string
}

type RateLimitError struct {
	StatusCode int
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitError) Error() string {
	return "Gemini rate limit exceeded"
}

type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Gemini API returned HTTP %d", e.StatusCode)
}

type Option func(*Provider)

func WithBaseURL(baseURL string) Option {
	return func(provider *Provider) {
		provider.baseURL = strings.TrimRight(baseURL, "/")
	}
}

func WithHTTPClient(client *http.Client) Option {
	return func(provider *Provider) {
		provider.client = client
	}
}

type Provider struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func New(apiKey string, options ...Option) *Provider {
	provider := &Provider{apiKey: apiKey, baseURL: defaultBaseURL, client: http.DefaultClient}
	for _, option := range options {
		option(provider)
	}
	if provider.client == nil {
		provider.client = http.DefaultClient
	}
	return provider
}

func (p *Provider) Locate(ctx context.Context, request GroundingRequest) ([]VisualMatch, error) {
	request.Description = strings.TrimSpace(request.Description)
	if request.Model == "" || request.Description == "" || !utf8.ValidString(request.Description) ||
		utf8.RuneCountInString(request.Description) > 500 ||
		len(request.Image) == 0 || len(request.Image) > 10<<20 ||
		request.Width < 1 || request.Width > 10_000 || request.Height < 1 || request.Height > 10_000 ||
		request.Limit < 1 || request.Limit > 10 {
		return nil, ErrInvalidGroundingRequest
	}
	instruction := fmt.Sprintf(
		"Locate up to %d visible matches for the requested UI target in this %dx%d screenshot. "+
			"Return only JSON as {\"matches\":[{\"x\":0,\"y\":0,\"confidence\":0.0,\"description\":\"\"}]}. "+
			"Coordinates are viewport pixels and confidence is between 0 and 1.",
		request.Limit, request.Width, request.Height,
	)
	modelRequest := agent.ModelRequest{
		Model:             agent.ModelRef{Provider: "gemini", Model: request.Model},
		SystemInstruction: instruction,
		Messages: []agent.Message{{
			Role: agent.RoleUser,
			Parts: []agent.MessagePart{
				{Text: &agent.TextPart{Text: request.Description}},
				{Image: &agent.ImagePart{Data: request.Image, MediaType: "image/png"}},
			},
		}},
		Generation: agent.GenerationOptions{JSONMode: true},
	}
	var output strings.Builder
	if err := p.Stream(ctx, modelRequest, func(event agent.ModelEvent) error {
		response, ok := event.(agent.ModelResponse)
		if !ok {
			return nil
		}
		for _, part := range response.Parts {
			if part.Text != nil {
				output.WriteString(part.Text.Text)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var decoded struct {
		Matches []VisualMatch
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("%w: grounding JSON: %v", ErrInvalidResponse, err)
	}
	if len(decoded.Matches) > request.Limit {
		return nil, fmt.Errorf("%w: too many grounding matches", ErrInvalidResponse)
	}
	return decoded.Matches, nil
}

func (p *Provider) Stream(ctx context.Context, request agent.ModelRequest, emit func(agent.ModelEvent) error) error {
	payload, err := payloadFor(request)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal Gemini request: %w", err)
	}
	endpoint, err := url.Parse(p.baseURL + "/v1beta/models/" + url.PathEscape(request.Model.Model) + ":streamGenerateContent")
	if err != nil {
		return fmt.Errorf("build Gemini URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("alt", "sse")
	endpoint.RawQuery = query.Encode()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Gemini request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("x-goog-api-key", p.apiKey)
	response, err := p.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("send Gemini request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		limitedBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return &RateLimitError{StatusCode: response.StatusCode, RetryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now()), Body: string(limitedBody)}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		limitedBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return &HTTPError{StatusCode: response.StatusCode, Body: string(limitedBody)}
	}

	return streamSSE(ctx, response.Body, emit)
}

func payloadFor(request agent.ModelRequest) (map[string]any, error) {
	systemParts := make([]any, 0)
	if request.SystemInstruction != "" {
		systemParts = append(systemParts, map[string]any{"text": request.SystemInstruction})
	}
	contents := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		parts, err := messageParts(message.Parts)
		if err != nil {
			return nil, err
		}
		if message.Role == agent.RoleSystem {
			systemParts = append(systemParts, parts...)
			continue
		}
		role := "user"
		if message.Role == agent.RoleAssistant {
			role = "model"
		} else if message.Role != agent.RoleUser && message.Role != agent.RoleTool {
			return nil, fmt.Errorf("unsupported Gemini message role %q", message.Role)
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	payload := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		payload["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	if len(request.Tools) > 0 {
		declarations := make([]any, 0, len(request.Tools))
		for _, tool := range request.Tools {
			var schema any
			if len(tool.InputSchema) == 0 {
				schema = map[string]any{}
			} else if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("tool %q input schema: %w", tool.Name, err)
			}
			if _, ok := schema.(map[string]any); !ok {
				return nil, fmt.Errorf("tool %q input schema must be a JSON object", tool.Name)
			}
			declarations = append(declarations, map[string]any{"name": tool.Name, "description": tool.Description, "parameters": schema})
		}
		payload["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	config := map[string]any{}
	if request.Generation.Temperature != nil {
		config["temperature"] = *request.Generation.Temperature
	}
	if request.Generation.MaxOutputTokens != nil {
		config["maxOutputTokens"] = *request.Generation.MaxOutputTokens
	}
	if request.Generation.JSONMode {
		config["responseMimeType"] = "application/json"
	}
	if len(config) > 0 {
		payload["generationConfig"] = config
	}
	return payload, nil
}

func messageParts(parts []agent.MessagePart) ([]any, error) {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch {
		case part.Text != nil:
			out = append(out, map[string]any{"text": part.Text.Text})
		case part.Image != nil:
			out = append(out, inlineData(part.Image.Data, part.Image.MediaType))
		case part.Audio != nil:
			out = append(out, inlineData(part.Audio.Data, part.Audio.MediaType))
		case part.ToolCall != nil:
			functionCall := map[string]any{"id": part.ToolCall.CallID, "name": part.ToolCall.Name, "args": part.ToolCall.Arguments}
			functionPart := map[string]any{"functionCall": functionCall}
			if signature, ok := part.ToolCall.ProviderData["thoughtSignature"].(string); ok {
				functionPart["thoughtSignature"] = signature
			}
			out = append(out, functionPart)
		case part.ToolResult != nil:
			result, err := normalizeFunctionResponse(part.ToolResult.Result)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"functionResponse": map[string]any{"id": part.ToolResult.CallID, "name": part.ToolResult.Name, "response": result}})
		default:
			return nil, errors.New("empty message part")
		}
	}
	return out, nil
}

func normalizeFunctionResponse(result any) (any, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	var normalized any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, fmt.Errorf("normalize tool result: %w", err)
	}
	if object, ok := normalized.(map[string]any); ok {
		return object, nil
	}
	return map[string]any{"result": result}, nil
}

func inlineData(data []byte, mediaType string) map[string]any {
	return map[string]any{"inlineData": map[string]any{"mimeType": mediaType, "data": base64.StdEncoding.EncodeToString(data)}}
}

func streamSSE(ctx context.Context, body io.Reader, emit func(agent.ModelEvent) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var dataLines []string
	var text strings.Builder
	var toolCalls []agent.ToolCallPart
	seenFinal := false
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			return nil
		}
		var chunk geminiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
		}
		for _, candidate := range chunk.Candidates {
			if candidate.Content == nil {
				continue
			}
			seenFinal = true
			for _, part := range candidate.Content.Parts {
				if part.Text != nil {
					text.WriteString(*part.Text)
					if err := emitModelEvent(emit, agent.TextDelta{Text: *part.Text}); err != nil {
						return err
					}
				}
				if part.FunctionCall != nil {
					if part.FunctionCall.Name == "" {
						return fmt.Errorf("%w: function call has no name", ErrInvalidResponse)
					}
					arguments := part.FunctionCall.Args
					if arguments == nil {
						arguments = map[string]any{}
					}
					callID := part.FunctionCall.ID
					if callID == "" {
						callID = fmt.Sprintf("gemini-%d", len(toolCalls)+1)
					}
					providerData := map[string]any{}
					if part.ThoughtSignature != "" {
						providerData["thoughtSignature"] = part.ThoughtSignature
					}
					toolCalls = append(toolCalls, agent.ToolCallPart{CallID: callID, Name: part.FunctionCall.Name, Arguments: arguments, ProviderData: providerData})
				}
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
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			dataLines = append(dataLines, strings.TrimPrefix(data, " "))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read Gemini stream: %w", err)
	}
	if err := flush(); err != nil {
		return err
	}
	if !seenFinal {
		return fmt.Errorf("%w: stream had no final response", ErrInvalidResponse)
	}
	parts := make([]agent.ResponsePart, 0, 1+len(toolCalls))
	if text.Len() > 0 {
		parts = append(parts, agent.ResponsePart{Text: &agent.TextPart{Text: text.String()}})
	}
	for index := range toolCalls {
		parts = append(parts, agent.ResponsePart{ToolCall: &toolCalls[index]})
	}
	return emitModelEvent(emit, agent.ModelResponse{Parts: parts})
}

func emitModelEvent(emit func(agent.ModelEvent) error, event agent.ModelEvent) error {
	if emit == nil {
		return nil
	}
	return emit(event)
}

type geminiChunk struct {
	Candidates []struct {
		Content *struct {
			Parts []struct {
				Text         *string `json:"text"`
				FunctionCall *struct {
					ID   string         `json:"id"`
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"functionCall"`
				ThoughtSignature string `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
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
