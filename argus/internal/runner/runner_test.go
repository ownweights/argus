package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/browser"
	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/policy"
	"github.com/ace-foundry/argus-testing/argus/internal/server"
	"github.com/ace-foundry/argus-testing/argus/internal/store"
)

type fakeFactory struct {
	session browser.Session
	open    func(context.Context) error
}

func (f fakeFactory) Open(ctx context.Context, _ ...browser.SessionOptions) (browser.Session, error) {
	if f.open != nil {
		if err := f.open(ctx); err != nil {
			return nil, err
		}
	}
	return f.session, nil
}

type fakeSession struct {
	browser.Session
	navigateErr     error
	screenshotErr   error
	closed          int
	screenshotPath  string
	screenshotData  [][]byte
	screenshotCount int
	calls           []string
	elements        map[string]browser.Element
	typedRef        string
	typedValue      string
	typedSensitive  bool
	inspectText     string
	pointElement    browser.Element
	pointClicks     int
	inspectWidth    int
	inspectHeight   int
}

func (s *fakeSession) Navigate(_ context.Context, url string) (browser.Navigation, error) {
	s.calls = append(s.calls, "navigate")
	return browser.Navigation{URL: url, Title: "Example"}, s.navigateErr
}
func (s *fakeSession) Inspect(context.Context) (browser.PageSnapshot, error) {
	s.calls = append(s.calls, "inspect")
	text := s.inspectText
	if text == "" {
		text = "private page text"
	}
	width, height := s.inspectWidth, s.inspectHeight
	if width == 0 {
		width = 800
	}
	if height == 0 {
		height = 600
	}
	return browser.PageSnapshot{URL: "https://example.com", Title: "Secret title", Text: text, Width: width, Height: height, Elements: []browser.Element{{Ref: "e1-1", Name: "private button"}}}, nil
}
func (s *fakeSession) Element(_ context.Context, reference string) (browser.Element, error) {
	if element, ok := s.elements[reference]; ok {
		element.Ref = reference
		return element, nil
	}
	return browser.Element{Ref: reference}, nil
}
func (s *fakeSession) ElementAt(context.Context, int, int) (browser.Element, error) {
	return s.pointElement, nil
}
func (s *fakeSession) Click(context.Context, string) (browser.ActionResult, error) {
	s.calls = append(s.calls, "click")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Type(_ context.Context, reference string, value browser.InputValue) (browser.ActionResult, error) {
	s.calls = append(s.calls, "type")
	s.typedRef = reference
	s.typedValue = value.Text
	s.typedSensitive = value.Sensitive
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) FillForm(_ context.Context, _ map[string]browser.InputValue) (browser.ActionResult, error) {
	s.calls = append(s.calls, "fill_form")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Submit(context.Context, string) (browser.ActionResult, error) {
	s.calls = append(s.calls, "submit")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Select(context.Context, string, string) (browser.ActionResult, error) {
	s.calls = append(s.calls, "select")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Press(context.Context, string) (browser.ActionResult, error) {
	s.calls = append(s.calls, "press")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Scroll(context.Context, int) (browser.ActionResult, error) {
	s.calls = append(s.calls, "scroll")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Resize(context.Context, int, int) (browser.ActionResult, error) {
	s.calls = append(s.calls, "resize")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Wait(context.Context, browser.WaitCondition) (browser.ActionResult, error) {
	s.calls = append(s.calls, "wait")
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) ConsoleErrors(context.Context) ([]string, error) {
	s.calls = append(s.calls, "console")
	return []string{"console failure"}, nil
}
func (s *fakeSession) NetworkErrors(context.Context) ([]browser.NetworkError, error) {
	s.calls = append(s.calls, "network")
	return []browser.NetworkError{{Method: "GET", URL: "https://example.com/missing", Status: 404}}, nil
}
func (s *fakeSession) ClickPoint(context.Context, int, int) (browser.ActionResult, error) {
	s.calls = append(s.calls, "click_point")
	s.pointClicks++
	return browser.ActionResult{URL: "https://example.com"}, nil
}
func (s *fakeSession) Screenshot(_ context.Context, path string) error {
	s.calls = append(s.calls, "screenshot")
	s.screenshotPath = path
	if s.screenshotErr != nil {
		return s.screenshotErr
	}
	data := []byte("png")
	if s.screenshotCount < len(s.screenshotData) {
		data = s.screenshotData[s.screenshotCount]
	}
	s.screenshotCount++
	return os.WriteFile(path, data, 0o644)
}
func (s *fakeSession) ScreenshotViewport(ctx context.Context, path string) error {
	return s.Screenshot(ctx, path)
}
func (s *fakeSession) Close() error { s.closed++; return nil }

type scriptedProvider struct {
	mu        sync.Mutex
	responses []agent.ModelResponse
	names     []string
	requests  []agent.ModelRequest
}

func (p *scriptedProvider) Stream(_ context.Context, request agent.ModelRequest, emit func(agent.ModelEvent) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.names = append(p.names, request.Model.Model+":"+request.SystemInstruction[:min(12, len(request.SystemInstruction))])
	p.requests = append(p.requests, request)
	if len(p.responses) == 0 {
		return errors.New("unexpected model call")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return emit(response)
}
func response(text string) agent.ModelResponse {
	return agent.ModelResponse{Parts: []agent.MessagePart{{Text: &agent.TextPart{Text: text}}}}
}
func tool(name string, values map[string]any) agent.ModelResponse {
	return agent.ModelResponse{Parts: []agent.MessagePart{{ToolCall: &agent.ToolCallPart{CallID: name, Name: name, Arguments: values}}}}
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestNewUsesDefaultModelWhenUnset(t *testing.T) {
	if model := New(nil, nil, Options{}).model.Model; model != "gemini-2.5-flash" {
		t.Fatalf("model = %q", model)
	}
}

func TestRunnerMapsPersistedProvidersAndReportsMissingConfiguration(t *testing.T) {
	db, _ := newTestStore(t)
	geminiProvider := &scriptedProvider{}
	gptProvider := &scriptedProvider{}
	runner := New(db, fakeFactory{}, Options{
		Provider: geminiProvider, GPTProvider: gptProvider,
		Model: "gemini-test", GPTModel: "gpt-vision-test",
	})
	model, _, missing := runner.configFor(domain.ProviderGPT)
	if model != (agent.ModelRef{Provider: "gpt", Model: "gpt-vision-test"}) || missing != "" {
		t.Fatalf("GPT config = %#v, %q", model, missing)
	}
	model, _, missing = runner.configFor(domain.ProviderKimi)
	if model.Provider != "kimi" || missing != "KIMI_API_KEY is not configured" {
		t.Fatalf("Kimi config = %#v, %q", model, missing)
	}
	model, _, missing = runner.configFor(domain.ProviderGLM)
	if model != (agent.ModelRef{Provider: "glm", Model: "glm-5.3-flash"}) || missing != "ZAI_API_KEY is not configured" {
		t.Fatalf("GLM config = %#v, %q", model, missing)
	}

	run, err := db.CreateRunWithProvider("https://example.com", "check", domain.ProviderKimi, domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddEvent(run.ID, domain.EventRunQueued, nil); err != nil {
		t.Fatal(err)
	}
	runner.Run(context.Background(), run.ID, domain.RunAuthorization{})
	got, err := db.GetRun(run.ID, false)
	if err != nil || got.Error == nil || *got.Error != "KIMI_API_KEY is not configured" {
		t.Fatalf("missing Kimi config run = %#v, %v", got, err)
	}
}

func newTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	t.Setenv("TYPESAFE_API_KEY", "")
	path := filepath.Join(t.TempDir(), "argus.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Initialize(); err != nil {
		t.Fatal(err)
	}
	return db, path
}
func queuedRun(t *testing.T, db *store.Store) *domain.Run {
	t.Helper()
	run, err := db.CreateRun("https://example.com", "check search", domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddEvent(run.ID, domain.EventRunQueued, nil); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunnerRunsPublicPipelineAndPersistsSafeBrowserEvents(t *testing.T) {
	db, databasePath := newTestStore(t)
	run := queuedRun(t, db)
	session := &fakeSession{screenshotData: [][]byte{[]byte("initial-shot"), []byte("pre-execution-shot"), []byte("tool-shot"), []byte("final-shot")}}
	provider := &scriptedProvider{responses: []agent.ModelResponse{
		response(`{"testable":true}`), response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`),
		tool("inspect_page", map[string]any{}), response(`{"pages":[{"url":"https://example.com","name":"Example","features":["Search"]}]}`),
		response(`{"cases":[{"id":"T1","name":"Search","steps":["Open search","Verify result"],"success":"A result is visible"}]}`),
		tool("click", map[string]any{"ref": "e1-1"}), tool("screenshot", map[string]any{"label": "Search result"}),
		response(fmt.Sprintf(`{"cases":[{"id":"T1","status":"passed","steps":["Clicked search","Observed result"],"findings":[],"evidence":["/screenshots/%s/search-result-3.png"]}],"summary":"Search passed"}`, run.ID)),
		response("```json\n{\"verdict\":\"passed\",\"summary\":\"verified\",\"findings\":[{\"severity\":\"info\",\"title\":\"ok\",\"detail\":\"works\"}],\"recommendations\":[\"keep it\"]}\n```"),
	}}
	r := New(db, fakeFactory{session: session}, Options{ScreenshotDir: filepath.Join(t.TempDir(), "screenshots"), Provider: provider})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, err := db.GetRun(run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != domain.RunStatusPassed || current.Report == nil || current.Report.Verdict != domain.ReportVerdictPassed {
		t.Fatalf("run = %#v", current)
	}
	if session.closed != 1 {
		t.Fatalf("close calls = %d", session.closed)
	}
	if len(provider.names) != 9 {
		t.Fatalf("model calls = %d", len(provider.names))
	}
	var screenshots, actions, observations int
	for _, event := range current.Events {
		switch event.Type {
		case domain.EventBrowserScreenshot:
			screenshots++
		case domain.EventBrowserAction:
			actions++
			if event.Data["tool"] == "type_text" {
				t.Fatalf("unexpected form-fill action = %#v", event)
			}
		case domain.EventBrowserObservation:
			observations++
			if event.Data["tool"] == "inspect_page" {
				result := event.Data["result"].(map[string]any)
				if _, ok := result["text"]; ok || result["summary"] != "Page inspection omitted from persisted events" {
					t.Fatalf("observation leaked: %#v", event)
				}
			}
		}
	}
	if screenshots != 3 || actions != 3 || observations != 3 {
		t.Fatalf("events screenshots/actions/observations = %d/%d/%d", screenshots, actions, observations)
	}
	for _, test := range []struct {
		index int
		image string
	}{{2, "initial-shot"}, {5, "pre-execution-shot"}, {7, "tool-shot"}, {8, "final-shot"}} {
		image := lastImage(provider.requests[test.index].Messages)
		if image == nil || image.MediaType != "image/png" || string(image.Data) != test.image {
			t.Fatalf("request %d image = %#v", test.index, image)
		}
	}
	for _, event := range current.Events {
		encoded, _ := json.Marshal(event)
		for _, value := range []string{"private page text", "private button", base64.StdEncoding.EncodeToString([]byte("initial-shot")), base64.StdEncoding.EncodeToString([]byte("pre-execution-shot")), base64.StdEncoding.EncodeToString([]byte("tool-shot")), base64.StdEncoding.EncodeToString([]byte("final-shot"))} {
			if strings.Contains(string(encoded), value) {
				t.Fatalf("event persisted private data: %s", encoded)
			}
		}
	}
	reader, err := sql.Open("sqlite", "file:"+databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.QueryRow("SELECT COUNT(*) FROM screenshots WHERE run_id = ?", run.ID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("screenshots = %d, %v", count, err)
	}
}

func lastImage(messages []agent.Message) *agent.ImagePart {
	for messageIndex := len(messages) - 1; messageIndex >= 0; messageIndex-- {
		parts := messages[messageIndex].Parts
		for partIndex := len(parts) - 1; partIndex >= 0; partIndex-- {
			if parts[partIndex].Image != nil {
				return parts[partIndex].Image
			}
		}
	}
	return nil
}

func TestRunnerRejectsTypeTextWithoutElementReference(t *testing.T) {
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	session := &fakeSession{}
	provider := &scriptedProvider{responses: []agent.ModelResponse{
		response(`{"testable":true}`), response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`),
		tool("type_text", map[string]any{"text": "credential"}),
	}}
	r := New(db, fakeFactory{session: session}, Options{ScreenshotDir: t.TempDir(), Provider: provider})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})

	current, err := db.GetRun(run.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != domain.RunStatusFailed {
		t.Fatalf("run status = %s", current.Status)
	}
	for _, call := range session.calls {
		if call == "type" {
			t.Fatal("Type was invoked for an unadvertised type_text call")
		}
	}
	if len(provider.requests) != 3 {
		t.Fatalf("model requests = %d", len(provider.requests))
	}
	advertised := false
	for _, browserTool := range provider.requests[2].Tools {
		advertised = advertised || browserTool.Name == "type_text"
	}
	if !advertised {
		t.Fatal("type_text was not advertised to the model")
	}
	for _, event := range current.Events {
		if event.Type == domain.EventBrowserAction && event.Data["tool"] == "type_text" {
			encoded, _ := json.Marshal(event)
			if bytes.Contains(encoded, []byte("credential")) {
				t.Fatalf("type_text event leaked input: %s", encoded)
			}
		}
	}
}

func TestBrowserAdapterReturnsClickAndNavigateObservations(t *testing.T) {
	adapter := &browserAdapter{session: &fakeSession{}}
	ctx := context.Background()

	navigate, err := adapter.navigate(ctx, map[string]any{"url": "https://example.com/a?view=all"}, agent.ToolContext{})
	navigation, _ := navigate.(map[string]any)
	if err != nil || navigation["action"] != "navigated" || navigation["url"] != "https://example.com/a?view=all" || navigation["snapshot"] == nil {
		t.Fatalf("navigate = %#v, %v", navigate, err)
	}
	click, err := adapter.click(ctx, map[string]any{"ref": "e1-1"}, agent.ToolContext{})
	clicked, _ := click.(map[string]any)
	if err != nil || clicked["action"] != "clicked" || clicked["url"] != "https://example.com" || clicked["snapshot"] == nil {
		t.Fatalf("click = %#v, %v", click, err)
	}
}

func TestTypeTextResolvesBindingAndReturnsOnlyRedactedMetadata(t *testing.T) {
	secrets, err := newSecretSet(map[string]string{"login_password": "secret-value"})
	if err != nil {
		t.Fatal(err)
	}
	defer secrets.Close()
	runPolicy, err := policy.New("https://example.com", domain.RunAuthorization{AllowMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{elements: map[string]browser.Element{"e1-1": {Mutating: true}}, inspectText: "signed in with secret-value"}
	adapter := &browserAdapter{session: session, policy: runPolicy, secrets: secrets}
	result, err := adapter.typeText(context.Background(), map[string]any{"ref": "e1-1", "secret": "login_password"}, agent.ToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	if session.typedRef != "e1-1" || session.typedValue != "secret-value" || !session.typedSensitive {
		t.Fatalf("typed = %q, %q, sensitive=%v", session.typedRef, session.typedValue, session.typedSensitive)
	}
	encoded, _ := json.Marshal(result)
	if bytes.Contains(encoded, []byte("secret-value")) || bytes.Contains(encoded, []byte("login_password")) {
		t.Fatalf("tool result leaked secret metadata: %s", encoded)
	}
}

func TestBrowserAdapterEnforcesNavigationAndActionPolicy(t *testing.T) {
	runPolicy, err := policy.New("https://example.com", domain.RunAuthorization{})
	if err != nil {
		t.Fatal(err)
	}
	session := &fakeSession{elements: map[string]browser.Element{
		"safe":     {},
		"mutating": {Mutating: true},
	}}
	adapter := &browserAdapter{session: session, policy: runPolicy}
	if _, err := adapter.click(context.Background(), map[string]any{"ref": "safe"}, agent.ToolContext{}); err != nil {
		t.Fatalf("safe click: %v", err)
	}
	if _, err := adapter.click(context.Background(), map[string]any{"ref": "mutating"}, agent.ToolContext{}); !errors.Is(err, policy.ErrMutationDenied) {
		t.Fatalf("mutating click error = %v", err)
	}
	if _, err := adapter.navigate(context.Background(), map[string]any{"url": "https://other.example/path"}, agent.ToolContext{}); !errors.Is(err, policy.ErrOriginDenied) {
		t.Fatalf("navigation error = %v", err)
	}
	if got := strings.Join(session.calls, ","); got != "click,inspect" {
		t.Fatalf("browser calls = %q", got)
	}
}

func TestExecutorAdvertisesCompleteSemanticToolSurface(t *testing.T) {
	tools := browserTools(&browserAdapter{}, true)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	want := []string{
		"inspect_page", "click", "type_text", "fill_form", "submit_form",
		"select_option", "press_key", "scroll", "resize_viewport", "wait_for",
		"console_errors", "network_errors", "navigate", "screenshot",
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("tool names = %#v, want %#v", names, want)
	}
}

func TestBrowserAdapterWiresCompleteSemanticToolSurface(t *testing.T) {
	runPolicy, err := policy.New("https://example.com", domain.RunAuthorization{AllowMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		values map[string]any
		invoke func(*browserAdapter, context.Context, map[string]any) (any, error)
		calls  string
	}{
		{"fill form", map[string]any{"fields": []any{map[string]any{"ref": "e1-1", "text": "Ada"}}}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.fillForm(ctx, values, agent.ToolContext{})
		}, "fill_form,inspect"},
		{"submit", map[string]any{"ref": "e1-1"}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.submitForm(ctx, values, agent.ToolContext{})
		}, "submit,inspect"},
		{"select", map[string]any{"ref": "e1-1", "value": "Winter"}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.selectOption(ctx, values, agent.ToolContext{})
		}, "select,inspect"},
		{"press", map[string]any{"key": "Enter"}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.pressKey(ctx, values, agent.ToolContext{})
		}, "press,inspect"},
		{"scroll", map[string]any{"delta_y": float64(500)}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.scroll(ctx, values, agent.ToolContext{})
		}, "scroll,inspect"},
		{"resize", map[string]any{"width": float64(375), "height": float64(812)}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.resize(ctx, values, agent.ToolContext{})
		}, "resize,inspect"},
		{"wait", map[string]any{"text": "Ready", "timeout_ms": float64(1000)}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.waitFor(ctx, values, agent.ToolContext{})
		}, "wait,inspect"},
		{"console", map[string]any{}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.consoleErrors(ctx, values, agent.ToolContext{})
		}, "console"},
		{"network", map[string]any{}, func(a *browserAdapter, ctx context.Context, values map[string]any) (any, error) {
			return a.networkErrors(ctx, values, agent.ToolContext{})
		}, "network"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &fakeSession{elements: map[string]browser.Element{"e1-1": {Mutating: true}}}
			adapter := &browserAdapter{session: session, policy: runPolicy}
			if _, err := test.invoke(adapter, context.Background(), test.values); err != nil {
				t.Fatal(err)
			}
			if calls := strings.Join(session.calls, ","); calls != test.calls {
				t.Fatalf("calls = %q, want %q", calls, test.calls)
			}
		})
	}
}

func TestReportNormalizationAndMissingKeyFailure(t *testing.T) {
	report := parseReport("not json", "plan", "execution")
	if report.Verdict != domain.ReportVerdictInconclusive || report.Summary != "not json" || report.Plan == nil {
		t.Fatalf("report = %#v", report)
	}
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	t.Setenv("GEMINI_API_KEY", "")
	New(db, fakeFactory{}, Options{ScreenshotDir: t.TempDir()}).Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, err := db.GetRun(run.ID, false)
	if err != nil || current.Status != domain.RunStatusFailed || current.Error == nil || *current.Error != missingAPIKey {
		t.Fatalf("run = %#v, %v", current, err)
	}
}

func TestRunnerDoesNotOverwriteCancellationRace(t *testing.T) {
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	opened := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := &scriptedProvider{responses: []agent.ModelResponse{response(`{"testable":true}`), response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`)}}
	r := New(db, fakeFactory{open: func(ctx context.Context) error { once.Do(func() { close(opened) }); <-ctx.Done(); return ctx.Err() }}, Options{ScreenshotDir: t.TempDir(), Provider: provider})
	done := make(chan struct{})
	go func() { r.Run(ctx, run.ID, domain.RunAuthorization{}); close(done) }()
	<-opened
	cancelled, err := db.Transition(run.ID, []domain.RunStatus{domain.RunStatusRunning}, domain.RunStatusCancelled, domain.EventRunCancelled, nil, nil, nil)
	if err != nil || cancelled == nil {
		t.Fatalf("cancel transition = %#v, %v", cancelled, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	current, err := db.GetRun(run.ID, true)
	if err != nil || current.Status != domain.RunStatusCancelled {
		t.Fatalf("run = %#v, %v", current, err)
	}
}

func TestNextScreenshotPathRejectsUnsafeRunIDs(t *testing.T) {
	for _, runID := range []string{".", "..", "a/b", "0123456789ABCDEF0123456789ABCDEF"} {
		if _, _, err := NextScreenshotPath(t.TempDir(), runID, "initial"); err == nil {
			t.Errorf("NextScreenshotPath(%q) error = nil", runID)
		}
	}
}
func TestNextScreenshotPathUsesSafeLabelAndSequence(t *testing.T) {
	root := t.TempDir()
	runID := "0123456789abcdef0123456789abcdef"
	public, disk, err := NextScreenshotPath(root, runID, " Initial / Page ")
	if err != nil {
		t.Fatal(err)
	}
	if public != "/screenshots/"+runID+"/initial---page-1.png" {
		t.Fatalf("public path = %q", public)
	}
	if err := os.WriteFile(disk, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	public, _, err = NextScreenshotPath(root, runID, "evidence")
	if err != nil {
		t.Fatal(err)
	}
	if public != "/screenshots/"+runID+"/evidence-2.png" {
		t.Fatalf("second public path = %q", public)
	}
}

func TestServerPostReachesTerminalReportWithFakePipeline(t *testing.T) {
	db, _ := newTestStore(t)
	provider := &scriptedProvider{responses: []agent.ModelResponse{
		response(`{"testable":true}`),
		response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`),
		response(`{"pages":[{"url":"https://example.com","name":"Example","features":["Search"]}]}`),
		response(`{"cases":[{"id":"T1","name":"Search","steps":["Open search"],"success":"A result is visible"}]}`),
		response(`{"cases":[{"id":"T1","status":"failed","steps":["Search was unavailable"],"findings":["Search control missing"],"evidence":[]}],"summary":"Search failed"}`),
		response(`{"verdict":"failed","summary":"Search could not be verified","findings":[{"severity":"high","title":"Search unavailable","detail":"The requested search flow could not be completed."}],"recommendations":["Restore the search control"]}`),
	}}
	r := New(db, fakeFactory{session: &fakeSession{}}, Options{ScreenshotDir: t.TempDir(), Provider: provider})
	handler, err := server.New(db, r, server.Options{GeminiConfigured: true})
	if err != nil {
		t.Fatal(err)
	}
	r.SetPublisher(handler.Publish)
	request := httptest.NewRequest(http.MethodPost, "/api/runs", strings.NewReader(`{"url":"https://example.com","instructions":"test search"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d", response.Code)
	}
	var created domain.Run
	if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, err := db.GetRun(created.ID, false)
		if err == nil && current != nil && current.Status == domain.RunStatusFailed && current.Report != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server run did not reach a terminal report")
}

func TestReportParserAcceptsFences(t *testing.T) {
	value := parseReport("```json\n{\"verdict\":\"failed\",\"summary\":\"x\"}\n```", "p", "o")
	encoded, _ := json.Marshal(value)
	if value.Verdict != domain.ReportVerdictFailed || len(encoded) == 0 {
		t.Fatalf("report = %#v", value)
	}
}

func TestReportParserFiltersMalformedEntries(t *testing.T) {
	value := parseReport(`{"verdict":"failed","summary":"verified","findings":[{"severity":"high","title":"broken","detail":"details"},null,{"severity":"low","title":3,"detail":"invalid"}],"recommendations":["retry",7,null]}`, "plan", "observations")
	if value.Verdict != domain.ReportVerdictFailed || value.Summary != "verified" || value.Plan == nil || *value.Plan != "plan" || len(value.Findings) != 1 || value.Findings[0].Title != "broken" || len(value.Recommendations) != 1 || value.Recommendations[0] != "retry" {
		t.Fatalf("report = %#v", value)
	}
}

func TestRunnerPersistsTimeoutKind(t *testing.T) {
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	provider := &scriptedProvider{responses: []agent.ModelResponse{response(`{"testable":true}`), response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`)}}
	r := New(db, fakeFactory{open: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}, Options{ScreenshotDir: t.TempDir(), Timeout: 10 * time.Millisecond, Provider: provider})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, err := db.GetRun(run.ID, true)
	if err != nil || current.Status != domain.RunStatusFailed {
		t.Fatalf("run = %#v, %v", current, err)
	}
	for _, event := range current.Events {
		if event.Type == domain.EventRunFailed && event.Data["kind"] == "timeout" {
			return
		}
	}
	t.Fatalf("timeout failure event = %#v", current.Events)
}
