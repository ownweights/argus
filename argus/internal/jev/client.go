package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidResponse = errors.New("invalid Jev response")

type Choice struct {
	Name       string
	Confidence float64
}

type Client struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

type Option func(*Client)

func WithModel(model string) Option {
	return func(c *Client) { c.model = model }
}

func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) { c.client = client }
}

func New(apiKey string, options ...Option) *Client {
	c := &Client{apiKey: apiKey, model: "jev-latest", baseURL: "https://api.typesafe.ai", client: &http.Client{
		Timeout:       3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
	for _, option := range options {
		option(c)
	}
	return c
}

func (c *Client) Choose(ctx context.Context, state any, instructions string, criteria map[string]string) (Choice, error) {
	endpoint, err := url.Parse(c.baseURL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return Choice{}, errors.New("invalid Jev base URL")
	}
	if len(criteria) == 0 || instructions == "" || c.apiKey == "" || c.model == "" {
		return Choice{}, errors.New("invalid Jev request")
	}
	payload := map[string]any{
		"model": c.model, "state": state,
		"questions": map[string]any{"decision": map[string]any{
			"type": "choice", "instructions": instructions, "criteria": criteria,
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Choice{}, fmt.Errorf("marshal Jev request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return Choice{}, fmt.Errorf("create Jev request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return Choice{}, fmt.Errorf("send Jev request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Choice{}, fmt.Errorf("Jev API returned HTTP %d", response.StatusCode)
	}
	var decoded struct {
		Answers map[string]struct {
			Type       string   `json:"type"`
			Name       string   `json:"choice"`
			Confidence *float64 `json:"confidence"`
		} `json:"answers"`
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return Choice{}, fmt.Errorf("read Jev response: %w", err)
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &decoded) != nil {
		return Choice{}, ErrInvalidResponse
	}
	answer := decoded.Answers["decision"]
	_, valid := criteria[answer.Name]
	if !valid || answer.Type != "choice" || answer.Confidence == nil || math.IsNaN(*answer.Confidence) || math.IsInf(*answer.Confidence, 0) || *answer.Confidence < 0 || *answer.Confidence > 1 {
		return Choice{}, ErrInvalidResponse
	}
	return Choice{Name: answer.Name, Confidence: *answer.Confidence}, nil
}
