package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChooseSendsTypedQuestionAndValidatesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var payload struct {
			Model     string            `json:"model"`
			State     map[string]string `json:"state"`
			Questions map[string]struct {
				Type         string            `json:"type"`
				Instructions string            `json:"instructions"`
				Criteria     map[string]string `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "jev-test" || payload.State["text"] != "Saved" || payload.Questions["decision"].Type != "choice" || payload.Questions["decision"].Criteria["yes"] != "Saved successfully" {
			t.Errorf("payload = %#v", payload)
		}
		_, _ = w.Write([]byte(`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":0.98}}}`))
	}))
	defer server.Close()
	client := New("test-key", WithBaseURL(server.URL), WithModel("jev-test"))
	choice, err := client.Choose(context.Background(), map[string]string{"text": "Saved"}, "Did saving succeed?", map[string]string{"yes": "Saved successfully"})
	if err != nil || choice.Name != "yes" || choice.Confidence != 0.98 {
		t.Fatalf("choice = %#v, %v", choice, err)
	}
}

func TestChooseRejectsMalformedAnswersAndSanitizesHTTPErrors(t *testing.T) {
	for _, body := range []string{
		`{}`, `{"answers":{"decision":{"type":"choice","choice":"unknown","confidence":1}}}`,
		`{"answers":{"decision":{"type":"choice","choice":"yes"}}}`,
		`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":1.1}}}`,
		`{"answers":{"decision":{"type":"noul","choice":"yes","confidence":1}}}`,
		`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":null}}}`,
		`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":-0.1}}}`,
		`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":1}}} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			_, err := New("key", WithBaseURL(server.URL)).Choose(context.Background(), "state", "question", map[string]string{"yes": "yes"})
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "private API key", http.StatusTooManyRequests)
	}))
	defer server.Close()
	_, err := New("key", WithBaseURL(server.URL)).Choose(context.Background(), "state", "question", map[string]string{"yes": "yes"})
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v", err)
	}
}

func TestChooseRejectsOversizedResponsesAndRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"decision":{"type":"choice","choice":"yes","confidence":1}}}` + strings.Repeat(" ", 1<<20)))
	}))
	defer server.Close()
	_, err := New("key", WithBaseURL(server.URL)).Choose(context.Background(), "state", "question", map[string]string{"yes": "yes"})
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("oversized response error = %v", err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" {
			t.Error("followed a redirect with API credentials")
		}
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	_, err = New("key", WithBaseURL(redirect.URL)).Choose(context.Background(), "state", "question", map[string]string{"yes": "yes"})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestChooseHonorsCancellationAndRejectsInvalidEndpoint(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New("key").Choose(ctx, "state", "question", map[string]string{"yes": "yes"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	_, err = New("key", WithBaseURL("https://user:password@example.com")).Choose(context.Background(), "state", "question", map[string]string{"yes": "yes"})
	if err == nil {
		t.Fatal("accepted credentials in endpoint")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = New("key", WithBaseURL(server.URL)).Choose(ctx, "state", "question", map[string]string{"yes": "yes"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}
