package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/store"
	"github.com/coder/websocket"
)

type fakeRunner struct {
	started   chan startedRun
	cancelled chan string
}

type startedRun struct {
	id            string
	authorization domain.RunAuthorization
}

func (r *fakeRunner) Run(ctx context.Context, id string, authorization domain.RunAuthorization) {
	r.started <- startedRun{id: id, authorization: authorization}
	<-ctx.Done()
	r.cancelled <- id
}

func newTestServer(t *testing.T, staticDir string) (*Server, *store.Store, *fakeRunner) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "argus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &fakeRunner{started: make(chan startedRun, 10), cancelled: make(chan string, 10)}
	server, err := New(db, runner, Options{StaticDir: staticDir, GeminiConfigured: true})
	if err != nil {
		t.Fatal(err)
	}
	return server, db, runner
}

func request(t *testing.T, client *http.Client, method, target, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func decode(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func TestRESTRoutesValidationAndCancellation(t *testing.T) {
	server, db, runner := newTestServer(t, "")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := httpServer.Client()
	for _, payload := range []string{`{"url":"file:///etc/passwd","instructions":"check"}`, `{"url":"https://user:secret@example.com","instructions":"check"}`, `{"url":"https://example.com/?access_token=x","instructions":"check"}`, `{"url":"https://example.com","instructions":""}`, `{"url":"https://example.com","instructions":"check","authorization":{"secret_bindings":{"not a binding":"private"}}}`} {
		response := request(t, client, http.MethodPost, httpServer.URL+"/api/runs", payload)
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d", response.StatusCode)
		}
		response.Body.Close()
	}
	response := request(t, client, http.MethodPost, httpServer.URL+"/api/runs", `{"url":"https://example.com/path?view=list","instructions":"check","authorization":{"allow_mutations":true,"allowed_origins":["https://accounts.example.com"],"secret_bindings":{"login_password":"private-value"}}}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var created domain.Run
	decode(t, response, &created)
	select {
	case got := <-runner.started:
		if got.id != created.ID || !got.authorization.AllowMutations || got.authorization.SecretBindings["login_password"] != "private-value" {
			t.Fatalf("started = %#v", got)
		}
		if created.Policy == nil || !created.Policy.AllowMutations || len(created.Policy.AllowedOrigins) != 1 {
			t.Fatalf("created policy = %#v", created.Policy)
		}
		encoded, _ := json.Marshal(created)
		if strings.Contains(string(encoded), "private-value") || strings.Contains(string(encoded), "login_password") {
			t.Fatalf("created run leaked secret binding: %s", encoded)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	time.Sleep(time.Millisecond)
	newer, err := db.CreateRun("https://example.com/newer", "check", domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, client, http.MethodGet, httpServer.URL+"/api/runs?limit=999", "")
	var listed []domain.Run
	decode(t, response, &listed)
	if len(listed) != 2 || listed[0].ID != newer.ID || listed[1].ID != created.ID {
		t.Fatalf("runs = %#v", listed)
	}
	response = request(t, client, http.MethodGet, httpServer.URL+"/api/runs?limit=0", "")
	decode(t, response, &listed)
	if len(listed) != 1 || listed[0].ID != newer.ID {
		t.Fatalf("clamped runs = %#v", listed)
	}
	response = request(t, client, http.MethodGet, httpServer.URL+"/api/runs/missing", "")
	var missing map[string]string
	decode(t, response, &missing)
	if response.StatusCode != http.StatusNotFound || missing["detail"] != "Run not found" {
		t.Fatalf("missing = %#v", missing)
	}
	response = request(t, client, http.MethodPost, httpServer.URL+"/api/runs/"+created.ID+"/cancel", "")
	var cancelled domain.Run
	decode(t, response, &cancelled)
	if cancelled.Status != domain.RunStatusCancelled {
		t.Fatalf("cancelled = %#v", cancelled)
	}
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("runner was not cancelled")
	}
	response = request(t, client, http.MethodPost, httpServer.URL+"/api/runs/"+created.ID+"/cancel", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("second cancel = %d", response.StatusCode)
	}
	response.Body.Close()
	response = request(t, client, http.MethodGet, httpServer.URL+"/api/settings", "")
	var settings domain.SettingsResponse
	decode(t, response, &settings)
	if !settings.GeminiConfigured || settings.Model != "gemini-2.5-flash" {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestWebSocketReplayLiveAndTerminalClose(t *testing.T) {
	server, db, _ := newTestServer(t, "")
	run, err := db.CreateRun("https://example.com", "check", domain.RunPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := db.AddEvent(run.ID, domain.EventRunQueued, nil)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	wsURL := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/ws/runs/" + url.PathEscape(run.ID)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, message, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var replay domain.RunEvent
	if err := json.Unmarshal(message, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != queued.ID {
		t.Fatalf("replay = %#v", replay)
	}
	terminal, err := db.Transition(run.ID, []domain.RunStatus{domain.RunStatusQueued}, domain.RunStatusPassed, domain.EventRunCompleted, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	server.Publish(*terminal)
	_, message, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var live domain.RunEvent
	if err := json.Unmarshal(message, &live); err != nil {
		t.Fatal(err)
	}
	if live.ID != terminal.ID {
		t.Fatalf("live = %#v", live)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close = %v", err)
	}
}

func TestStaticFallbackDoesNotSwallowAPI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "asset.txt"), []byte("asset"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, _, _ := newTestServer(t, dir)
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	response := request(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/dashboard", "")
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "app" {
		t.Fatalf("fallback = %q", body)
	}
	response = request(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/asset.txt", "")
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "asset" {
		t.Fatalf("asset = %q", body)
	}
	for _, path := range []string{"/api", "/api/unknown"} {
		response = request(t, httpServer.Client(), http.MethodGet, httpServer.URL+path, "")
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("api status for %s = %d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

func TestCloseCancelsActiveRuns(t *testing.T) {
	server, _, runner := newTestServer(t, "")
	if !server.start("active", domain.RunAuthorization{}, nil) {
		t.Fatal("runner did not start")
	}
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	server.Close()
	server.Wait()
	select {
	case <-runner.cancelled:
	case <-time.After(time.Second):
		t.Fatal("runner was not cancelled")
	}
}

func TestAdmittedRunStartsAfterClose(t *testing.T) {
	server, _, runner := newTestServer(t, "")
	admission := server.admitRun()
	if admission == nil {
		t.Fatal("run was not admitted")
	}
	server.Close()
	if !server.start("admitted", domain.RunAuthorization{}, admission) {
		t.Fatal("admitted runner did not start")
	}
	select {
	case id := <-runner.started:
		if id.id != "admitted" {
			t.Fatalf("started = %#v", id)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}
	server.cancelTask("admitted")
	server.Wait()
}

func TestCloseRejectsNewRunsBeforePersistence(t *testing.T) {
	server, db, runner := newTestServer(t, "")
	server.Close()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	response := request(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/runs", `{"url":"https://example.com","instructions":"check"}`)
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var responseBody map[string]string
	decode(t, response, &responseBody)
	if responseBody["detail"] != "Server is shutting down" {
		t.Fatalf("response = %#v", responseBody)
	}
	runs, err := db.ListRuns(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v", runs)
	}
	select {
	case id := <-runner.started:
		t.Fatalf("runner started %#v", id)
	default:
	}
}

func TestMissingRunWebSocketCloses4404(t *testing.T) {
	server, _, _ := newTestServer(t, "")
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/ws/runs/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusCode(4404) {
		t.Fatalf("close = %v", err)
	}
}

func TestURLSensitiveNameNormalization(t *testing.T) {
	for _, value := range []string{"https://example.com/?x-api-key=x", "https://example.com/?sessionToken=x", "https://example.com/?client_secret=x", "https://example.com/?access%ZZ_token=x"} {
		if err := ValidateURL(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestCreateRequestAuthorizationDefaultsReadOnly(t *testing.T) {
	got, validation := decodeCreateRequest(strings.NewReader(`{"url":"https://example.com","instructions":"inspect"}`))
	if len(validation) != 0 {
		t.Fatalf("validation = %#v", validation)
	}
	if got.Authorization != nil {
		t.Fatalf("authorization = %#v", got.Authorization)
	}
}

func TestCreateRunProviderSelectionAndSettingsCatalog(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "argus.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	runner := &fakeRunner{started: make(chan startedRun, 10), cancelled: make(chan string, 10)}
	server, err := New(db, runner, Options{GeminiConfigured: true, OpenAIConfigured: true, GLMConfigured: true})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	var gptRunID string
	for _, test := range []struct {
		payload string
		want    domain.ProviderID
	}{
		{`{"url":"https://example.com","instructions":"check"}`, domain.ProviderGemini},
		{`{"url":"https://example.com","instructions":"check","provider":"gpt"}`, domain.ProviderGPT},
		{`{"url":"https://example.com","instructions":"check","provider":"glm"}`, domain.ProviderGLM},
	} {
		response := request(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/runs", test.payload)
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("create %s = %d", test.payload, response.StatusCode)
		}
		var run domain.Run
		decode(t, response, &run)
		if run.Provider != test.want {
			t.Fatalf("provider = %q, want %q", run.Provider, test.want)
		}
		if test.want == domain.ProviderGPT {
			gptRunID = run.ID
		}
	}
	response := request(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/runs/"+gptRunID, "")
	var persisted domain.Run
	decode(t, response, &persisted)
	if persisted.Provider != domain.ProviderGPT {
		t.Fatalf("persisted provider = %q", persisted.Provider)
	}
	for _, value := range []string{"kimi", "unknown"} {
		response := request(t, httpServer.Client(), http.MethodPost, httpServer.URL+"/api/runs", `{"url":"https://example.com","instructions":"check","provider":"`+value+`"}`)
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("provider %q status = %d", value, response.StatusCode)
		}
		var validation validationResponse
		decode(t, response, &validation)
		if len(validation.Detail) != 1 || !sameLoc(validation.Detail[0].Loc, []any{"body", "provider"}) {
			t.Fatalf("provider %q validation = %#v", value, validation)
		}
	}
	runs, err := db.ListRuns(100)
	if err != nil || len(runs) != 3 {
		t.Fatalf("runs after rejected providers = %#v, %v", runs, err)
	}

	response = request(t, httpServer.Client(), http.MethodGet, httpServer.URL+"/api/settings", "")
	var settings domain.SettingsResponse
	decode(t, response, &settings)
	want := []domain.ProviderInfo{
		{ID: domain.ProviderGemini, Available: true, Default: true},
		{ID: domain.ProviderGPT, Available: true},
		{ID: domain.ProviderKimi, Available: false},
		{ID: domain.ProviderGLM, Available: true},
	}
	if !reflect.DeepEqual(settings.Providers, want) {
		t.Fatalf("providers = %#v, want %#v", settings.Providers, want)
	}
}
