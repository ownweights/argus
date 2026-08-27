package domain

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRunJSONContract(t *testing.T) {
	fixture := []byte(`{"id":"run-123","url":"https://example.com","instructions":"Check navigation","provider":"gpt","status":"passed","created_at":"2025-01-02T03:04:05+00:00","updated_at":"2025-01-02T03:05:06+00:00","error":null,"report":{"verdict":"passed","summary":"Navigation works","plan":"Open the site","findings":[{"severity":"low","title":"Minor issue","detail":"Small visual mismatch"}],"recommendations":["Adjust spacing"]},"events":[{"id":7,"run_id":"run-123","type":"run.completed","data":{"verdict":"passed"},"created_at":"2025-01-02T03:05:06+00:00"}]}`)

	var run Run
	if err := json.Unmarshal(fixture, &run); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, fixture, encoded)
}

func TestOptionalFieldsAreOmitted(t *testing.T) {
	run := Run{
		ID:           "run-123",
		URL:          "https://example.com",
		Instructions: "Check navigation",
		Provider:     ProviderGemini,
		Status:       RunStatusQueued,
		CreatedAt:    "2025-01-02T03:04:05+00:00",
		UpdatedAt:    "2025-01-02T03:04:05+00:00",
		Error:        nil,
		Report: &RunReport{
			Verdict:         ReportVerdictInconclusive,
			Summary:         "Need more evidence",
			Findings:        []Finding{},
			Recommendations: []string{},
		},
	}

	encoded, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, []byte(`{"id":"run-123","url":"https://example.com","instructions":"Check navigation","provider":"gemini","status":"queued","created_at":"2025-01-02T03:04:05+00:00","updated_at":"2025-01-02T03:04:05+00:00","error":null,"report":{"verdict":"inconclusive","summary":"Need more evidence","findings":[],"recommendations":[]}}`), encoded)
}

func TestRequestAndSettingsJSONContracts(t *testing.T) {
	request, err := json.Marshal(CreateRequest{URL: "https://example.com", Instructions: "Check navigation", Provider: ProviderGLM})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, []byte(`{"url":"https://example.com","instructions":"Check navigation","provider":"glm"}`), request)

	settings, err := json.Marshal(SettingsResponse{GeminiConfigured: true, Model: "gemini-2.5-flash", Providers: []ProviderInfo{{ID: ProviderGemini, Available: true, Default: true}, {ID: ProviderGPT, Available: false}, {ID: ProviderKimi, Available: true}, {ID: ProviderGLM, Available: false}}})
	if err != nil {
		t.Fatal(err)
	}
	assertJSONEqual(t, []byte(`{"gemini_configured":true,"model":"gemini-2.5-flash","providers":[{"id":"gemini","available":true,"default":true},{"id":"gpt","available":false,"default":false},{"id":"kimi","available":true,"default":false},{"id":"glm","available":false,"default":false}]}`), settings)
}

func TestContractConstants(t *testing.T) {
	if got, want := []RunStatus{RunStatusQueued, RunStatusRunning, RunStatusPassed, RunStatusFailed, RunStatusCancelled}, []RunStatus{"queued", "running", "passed", "failed", "cancelled"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("run statuses = %v, want %v", got, want)
	}
	if got, want := []ReportVerdict{ReportVerdictPassed, ReportVerdictFailed, ReportVerdictInconclusive}, []ReportVerdict{"passed", "failed", "inconclusive"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("report verdicts = %v, want %v", got, want)
	}
	if got, want := []string{RunsEndpoint, RunEndpoint, CancelRunEndpoint, SettingsEndpoint, RunEventsEndpoint}, []string{"/api/runs", "/api/runs/{run_id}", "/api/runs/{run_id}/cancel", "/api/settings", "/ws/runs/{run_id}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("endpoints = %v, want %v", got, want)
	}
	if got, want := []EventType{EventRunQueued, EventRunStarted, EventRunCompleted, EventRunFailed, EventRunCancelled, EventPlanCompleted, EventBrowserAction, EventBrowserObservation, EventBrowserScreenshot}, []EventType{"run.queued", "run.started", "run.completed", "run.failed", "run.cancelled", "plan.completed", "browser.action", "browser.observation", "browser.screenshot"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
}

func assertJSONEqual(t *testing.T, want, got []byte) {
	t.Helper()

	var wantValue, gotValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\ngot:  %s\nwant: %s", got, want)
	}
}
