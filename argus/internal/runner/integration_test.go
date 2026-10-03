package runner

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/browser"
	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/jev"
)

type stageProvider struct {
	responses map[string]agent.ModelResponse
	requests  []agent.ModelRequest
}

func (p *stageProvider) Stream(_ context.Context, request agent.ModelRequest, emit func(agent.ModelEvent) error) error {
	p.requests = append(p.requests, request)
	response, ok := p.responses[request.SystemInstruction]
	if !ok {
		return fmt.Errorf("unexpected stubbed model stage")
	}
	return emit(response)
}

func TestIntegrationJevUsesRealPlaywright(t *testing.T) {
	if os.Getenv("ARGUS_PLAYWRIGHT_SMOKE") != "1" {
		t.Skip("set ARGUS_PLAYWRIGHT_SMOKE=1 after installing Chromium")
	}
	fixture, err := os.ReadFile(filepath.Join("..", "browser", "testdata", "fixture.html"))
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(fixture) }))
	defer app.Close()
	client, requests := testJev(t, jevReply{"testable", .99, 0}, jevReply{"e1-2", .99, 0}, jevReply{"e3-4", .99, 0}, jevReply{"satisfied", .99, 0})
	live := os.Getenv("ARGUS_JEV_LIVE") == "1"
	if live {
		if os.Getenv("TYPESAFE_API_KEY") == "" {
			t.Fatal("ARGUS_JEV_LIVE requires TYPESAFE_API_KEY")
		}
		client = jev.New(os.Getenv("TYPESAFE_API_KEY"))
	}
	provider := &stageProvider{responses: map[string]agent.ModelResponse{
		comprehenderInstruction:                           response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`),
		explorerInstruction:                               response(`{"pages":[{"url":"https://example.com","name":"Example","features":["Search"]}]}`),
		strategistInstruction + "\n" + domPlanInstruction: response(domSearchPlan),
		executorInstruction:                               response(`{"cases":[{"id":"T1","status":"inconclusive","steps":[],"findings":[],"evidence":[]}],"summary":"Needs independent review"}`),
		criticInstruction:                                 response(`{"verdict":"passed","summary":"Search reviewed","findings":[],"recommendations":[]}`),
	}}
	db, _ := newTestStore(t)
	run, err := db.CreateRun(app.URL, "Search for Acme and verify Acme appears in the result", domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	r := New(db, browser.NewPlaywrightFactory(), Options{Provider: provider, Jev: client, ScreenshotDir: t.TempDir(), Timeout: 30 * time.Second})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, err := db.GetRun(run.ID, true)
	if err != nil || current.Report == nil || current.Report.Verdict == domain.ReportVerdictFailed {
		t.Fatalf("run = %#v, %v", current, err)
	}
	if !live && (current.Report.Verdict != domain.ReportVerdictPassed || len(provider.requests) != 4) {
		t.Fatalf("expected a passing run with comprehender, explorer, strategist and critic only; got %d calls", len(provider.requests))
	}
	if !live && !strings.Contains(strings.Join(*requests, ""), "Acme — All") {
		t.Fatal("Jev did not receive the actual browser result")
	}
	var assertions, actions int
	for _, event := range current.Events {
		if event.Type == domain.EventBrowserObservation && event.Data["tool"] == "jev_assertion" {
			assertions++
			if live {
				t.Logf("Live assertion: %v; model calls: %d; verdict: %s", event.Data["result"], len(provider.requests), current.Report.Verdict)
			}
		}
		if event.Type == domain.EventBrowserAction {
			actions++
		}
	}
	if assertions != 1 || actions != 2 {
		t.Fatalf("assertion decisions/actions = %d/%d", assertions, actions)
	}
}

func TestIntegrationFullRunnerUsesRealPlaywrightAndEvidence(t *testing.T) {
	if os.Getenv("ARGUS_PLAYWRIGHT_SMOKE") != "1" {
		t.Skip("set ARGUS_PLAYWRIGHT_SMOKE=1 after running argus install-browser")
	}
	fixture, err := os.ReadFile(filepath.Join("..", "browser", "testdata", "fixture.html"))
	if err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/":
			_, _ = w.Write(fixture)
		case "/api/save":
			_, _ = w.Write([]byte("Preference saved"))
		case "/missing":
			http.Error(w, "missing", http.StatusNotFound)
		default:
			http.NotFound(w, request)
		}
	}))
	defer app.Close()

	db, _ := newTestStore(t)
	run, err := db.CreateRun(app.URL, "Search for Airbnb and verify the visible result", domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddEvent(run.ID, domain.EventRunQueued, nil); err != nil {
		t.Fatal(err)
	}
	evidencePath := fmt.Sprintf("/screenshots/%s/integration-result-3.png", run.ID)
	provider := &scriptedProvider{responses: []agent.ModelResponse{
		response("{\"testable\":true}"),
		response("{\"objective\":\"Verify company search\",\"features\":[\"Company search\"],\"constraints\":[\"Read-only\"]}"),
		tool("inspect_page", map[string]any{}),
		response(fmt.Sprintf("{\"pages\":[{\"url\":%q,\"name\":\"QA fixture\",\"features\":[\"Company search\"]}]}", app.URL)),
		response("{\"cases\":[{\"id\":\"T1\",\"name\":\"Company search\",\"steps\":[\"Enter Airbnb\",\"Show result\",\"Capture evidence\"],\"success\":\"Airbnb is visible in the result\"}]}"),
		tool("type_text", map[string]any{"ref": "e1-2", "text": "Airbnb"}),
		tool("click", map[string]any{"ref": "e2-4"}),
		tool("screenshot", map[string]any{"label": "Integration result"}),
		response(fmt.Sprintf("{\"cases\":[{\"id\":\"T1\",\"status\":\"passed\",\"steps\":[\"Entered Airbnb\",\"Displayed result\"],\"findings\":[],\"evidence\":[%q]}],\"summary\":\"Company search passed\"}", evidencePath)),
		response("{\"verdict\":\"passed\",\"summary\":\"Company search was positively verified\",\"findings\":[],\"recommendations\":[]}"),
	}}
	runner := New(db, browser.NewPlaywrightFactory(), Options{
		ScreenshotDir: filepath.Join(t.TempDir(), "screenshots"),
		Timeout:       30 * time.Second,
		Provider:      provider,
	})
	runner.Run(context.Background(), run.ID, domain.RunAuthorization{})

	current, err := db.GetRun(run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != domain.RunStatusPassed || current.Report == nil || current.Report.Verdict != domain.ReportVerdictPassed {
		t.Fatalf("run = %#v", current)
	}
	var typeActions, clickActions, screenshots int
	for _, event := range current.Events {
		switch {
		case event.Type == domain.EventBrowserAction && event.Data["tool"] == "type_text":
			typeActions++
		case event.Type == domain.EventBrowserAction && event.Data["tool"] == "click":
			clickActions++
		case event.Type == domain.EventBrowserScreenshot:
			screenshots++
		}
	}
	if typeActions != 1 || clickActions != 1 || screenshots != 3 {
		t.Fatalf("actions/screenshots = %d/%d/%d", typeActions, clickActions, screenshots)
	}
}
