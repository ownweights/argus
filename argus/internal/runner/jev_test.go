package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/browser"
	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/jev"
	"github.com/ace-foundry/argus-testing/argus/internal/policy"
)

type jevReply struct {
	choice     string
	confidence float64
	status     int
}

func testJev(t *testing.T, replies ...jevReply) (*jev.Client, *[]string) {
	t.Helper()
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests = append(requests, string(payload))
		if len(replies) == 0 {
			t.Error("unexpected Jev call")
			http.Error(w, "unexpected", 500)
			return
		}
		reply := replies[0]
		replies = replies[1:]
		if reply.status != 0 {
			w.WriteHeader(reply.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"decision": map[string]any{"type": "choice", "choice": reply.choice, "confidence": reply.confidence}}})
	}))
	t.Cleanup(server.Close)
	return jev.New("test-key", jev.WithBaseURL(server.URL)), &requests
}

type domSession struct {
	*fakeSession
	generation int
	refs       []string
	clickErr   error
}

func (s *domSession) Inspect(ctx context.Context) (browser.PageSnapshot, error) {
	value, err := s.fakeSession.Inspect(ctx)
	s.generation++
	value.Elements = []browser.Element{
		{Ref: fmt.Sprintf("e%d-1", s.generation), Tag: "input", Label: "Company search", InputType: "text"},
		{Ref: fmt.Sprintf("e%d-2", s.generation), Tag: "button", Name: "Show result"},
	}
	return value, err
}
func (s *domSession) Type(ctx context.Context, ref string, value browser.InputValue) (browser.ActionResult, error) {
	s.refs = append(s.refs, ref)
	return s.fakeSession.Type(ctx, ref, value)
}
func (s *domSession) Click(ctx context.Context, ref string) (browser.ActionResult, error) {
	s.refs = append(s.refs, ref)
	result, err := s.fakeSession.Click(ctx, ref)
	if s.clickErr != nil {
		return result, s.clickErr
	}
	return result, err
}

const domSearchPlan = `{"cases":[{"id":"T1","name":"Search","steps":["Enter Acme","Show result"],"success":"Acme is visible","dom":{"actions":[{"tool":"type_text","target":"Company search","text":"Acme"},{"tool":"click","target":"Show result"}],"assertions":["Acme is visible in the search result"]}}]}`

func domProvider(extra ...agent.ModelResponse) *scriptedProvider {
	responses := []agent.ModelResponse{
		response(`{"objective":"Verify search","features":["Search"],"constraints":["Read-only"]}`),
		response(`{"pages":[{"url":"https://example.com","name":"Example","features":["Search"]}]}`),
		response(domSearchPlan),
	}
	return &scriptedProvider{responses: append(responses, extra...)}
}

func TestJevReplacesValidatorAndExecutorButKeepsCriticAndEvidence(t *testing.T) {
	client, requests := testJev(t, jevReply{"testable", .99, 0}, jevReply{"e1-1", .99, 0}, jevReply{"e3-2", .99, 0}, jevReply{"satisfied", .99, 0})
	provider := domProvider(response(`{"verdict":"passed","summary":"Search verified","findings":[],"recommendations":[]}`))
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	session := &domSession{fakeSession: &fakeSession{}}
	r := New(db, fakeFactory{session: session}, Options{Provider: provider, Jev: client, ScreenshotDir: t.TempDir()})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, err := db.GetRun(run.ID, true)
	if err != nil || current.Report == nil || current.Report.Verdict != domain.ReportVerdictPassed {
		t.Fatalf("run = %#v, %v", current, err)
	}
	if len(provider.requests) != 4 || len(*requests) != 4 || strings.Join(session.refs, ",") != "e1-1,e3-2" {
		t.Fatalf("LLM/Jev/refs = %d/%d/%v", len(provider.requests), len(*requests), session.refs)
	}
	if !strings.Contains(provider.requests[2].SystemInstruction, `"dom"`) {
		t.Fatal("structured plan was not requested")
	}
	var screenshots, actions int
	for _, event := range current.Events {
		if event.Type == domain.EventBrowserScreenshot {
			screenshots++
		}
		if event.Type == domain.EventBrowserAction {
			actions++
		}
		encoded, _ := json.Marshal(event)
		if strings.Contains(string(encoded), "private page text") {
			t.Fatal("page text persisted")
		}
	}
	if screenshots < 3 || actions != 2 {
		t.Fatalf("screenshots/actions = %d/%d", screenshots, actions)
	}
}

func TestJevValidatorFallbackAndRejection(t *testing.T) {
	for _, reply := range []jevReply{{"testable", .4, 0}, {"", 0, 429}, {"not_testable", .99, 0}} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			client, _ := testJev(t, reply)
			provider := &scriptedProvider{responses: []agent.ModelResponse{response(`{"testable":false,"reason":"Not a test request"}`)}}
			r := New(nil, nil, Options{Provider: provider, Jev: client})
			ok, _, err := r.validateRequest(context.Background(), "", &domain.Run{URL: "https://example.com", Instructions: "hello"}, nil)
			if err != nil || ok {
				t.Fatalf("validation = %v, %v", ok, err)
			}
			wantCalls := 1
			if reply.choice == "not_testable" {
				wantCalls = 0
			}
			if len(provider.requests) != wantCalls {
				t.Fatalf("fallback calls = %d", len(provider.requests))
			}
		})
	}
}

func TestJevFallbackResumesOnlyUnfinishedActions(t *testing.T) {
	client, _ := testJev(t, jevReply{"testable", .99, 0}, jevReply{"e1-1", .99, 0}, jevReply{"", 0, 503})
	provider := domProvider(
		tool("click", map[string]any{"ref": "e3-2"}),
		response(`{"cases":[{"id":"T1","status":"inconclusive","steps":["Clicked Show result"],"findings":[],"evidence":[]}],"summary":"Needs review"}`),
		response(`{"verdict":"inconclusive","summary":"Needs review","findings":[],"recommendations":[]}`),
	)
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	session := &domSession{fakeSession: &fakeSession{}}
	r := New(db, fakeFactory{session: session}, Options{Provider: provider, Jev: client, ScreenshotDir: t.TempDir()})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, _ := db.GetRun(run.ID, false)
	if current.Report == nil {
		t.Fatalf("run = %#v", current)
	}
	if strings.Count(strings.Join(session.calls, ","), "type") != 1 || strings.Count(strings.Join(session.calls, ","), "click") != 1 {
		t.Fatalf("actions replayed: %v", session.calls)
	}
	message := provider.requests[3].Messages[0].Parts[0].Text.Text
	if !strings.Contains(message, "Do not repeat completed actions") || !strings.Contains(message, `"actions":[{"tool":"click"`) {
		t.Fatalf("resume message = %s", message)
	}
	critic := provider.requests[len(provider.requests)-1].Messages[0].Parts[0].Text.Text
	if !strings.Contains(critic, "type_text") {
		t.Fatal("completed fast-path steps were lost")
	}
}

func TestJevAssertionFallbackCannotReplayCompletedActions(t *testing.T) {
	client, _ := testJev(t, jevReply{"testable", .99, 0}, jevReply{"e1-1", .99, 0}, jevReply{"e3-2", .99, 0}, jevReply{"insufficient_evidence", .99, 0})
	provider := domProvider(
		response(`{"cases":[{"id":"T1","status":"inconclusive","steps":[],"findings":[],"evidence":[]}],"summary":"Needs visual review"}`),
		response(`{"verdict":"inconclusive","summary":"Needs visual review","findings":[],"recommendations":[]}`),
	)
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	r := New(db, fakeFactory{session: &domSession{fakeSession: &fakeSession{}}}, Options{Provider: provider, Jev: client, ScreenshotDir: t.TempDir()})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, _ := db.GetRun(run.ID, false)
	if current.Report == nil {
		t.Fatalf("run = %#v", current)
	}
	for _, tool := range provider.requests[3].Tools {
		if tool.Name == "click" || tool.Name == "type_text" || tool.Name == "submit_form" {
			t.Fatalf("verification fallback can replay actions: %s", tool.Name)
		}
	}
}

func TestJevFastPathPreservesPolicyAndRedaction(t *testing.T) {
	client, requests := testJev(t, jevReply{"e1-1", .99, 0})
	secrets, _ := newSecretSet(map[string]string{"password": "super-secret"})
	defer secrets.Close()
	p, _ := policy.New("https://example.com", domain.RunAuthorization{})
	session := &domSession{fakeSession: &fakeSession{inspectText: "super-secret", elements: map[string]browser.Element{"e1-1": {Mutating: true}}}}
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	a := &browserAdapter{runner: New(db, nil, Options{Jev: client}), runID: run.ID, session: session, policy: p, secrets: secrets}
	var plan TestPlan
	if err := decodeContract(domSearchPlan, &plan); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := a.runner.executeDOM(context.Background(), a, plan.Cases[0])
	if !errors.Is(err, policy.ErrMutationDenied) {
		t.Fatalf("policy error = %v", err)
	}
	if strings.Contains(strings.Join(*requests, ""), "super-secret") || session.typedValue != "" {
		t.Fatal("secret leaked or denied action executed")
	}
}

func TestJevBrowserFailureDoesNotTriggerActionReplay(t *testing.T) {
	client, _ := testJev(t, jevReply{"testable", .99, 0}, jevReply{"e1-1", .99, 0}, jevReply{"e3-2", .99, 0})
	provider := domProvider()
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	session := &domSession{fakeSession: &fakeSession{}, clickErr: errors.New("uncertain browser outcome")}
	r := New(db, fakeFactory{session: session}, Options{Provider: provider, Jev: client, ScreenshotDir: t.TempDir()})
	r.Run(context.Background(), run.ID, domain.RunAuthorization{})
	current, _ := db.GetRun(run.ID, false)
	if current.Error == nil || len(provider.requests) != 3 || strings.Count(strings.Join(session.calls, ","), "click") != 1 {
		t.Fatalf("run/calls = %#v/%v", current, session.calls)
	}
}

func TestJevUncertainTargetsAndUnsupportedPlansDoNotExecute(t *testing.T) {
	for _, reply := range []jevReply{{"e1-1", .5, 0}, {"not_found", .99, 0}, {"stale-ref", .99, 0}} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			client, _ := testJev(t, reply)
			var plan TestPlan
			if err := decodeContract(domSearchPlan, &plan); err != nil {
				t.Fatal(err)
			}
			session := &domSession{fakeSession: &fakeSession{}}
			r := New(nil, nil, Options{Jev: client})
			_, next, complete, err := r.executeDOM(context.Background(), &browserAdapter{runner: r, session: session}, plan.Cases[0])
			if err != nil || complete || next != 0 || len(session.refs) != 0 {
				t.Fatalf("next/complete/error/refs = %d/%v/%v/%v", next, complete, err, session.refs)
			}
		})
	}
	client, requests := testJev(t)
	var plan TestPlan
	if err := decodeContract(strings.Replace(domSearchPlan, `"tool":"click"`, `"tool":"scroll"`, 1), &plan); err != nil {
		t.Fatal(err)
	}
	session := &domSession{fakeSession: &fakeSession{}}
	r := New(nil, nil, Options{Jev: client})
	_, next, complete, err := r.executeDOM(context.Background(), &browserAdapter{runner: r, session: session}, plan.Cases[0])
	if err != nil || complete || next != 0 || len(*requests) != 0 || len(session.calls) != 0 {
		t.Fatal("unsupported case was partially executed")
	}
}

func TestJevOutcomesRequireConfidenceAndPersistedEvidence(t *testing.T) {
	for _, reply := range []jevReply{{"satisfied", .99, 0}, {"not_satisfied", .99, 0}, {"satisfied", .5, 0}} {
		t.Run(fmt.Sprint(reply), func(t *testing.T) {
			client, _ := testJev(t, reply)
			db, _ := newTestStore(t)
			run := queuedRun(t, db)
			r := New(db, nil, Options{Jev: client, ScreenshotDir: t.TempDir()})
			a := &browserAdapter{runner: r, runID: run.ID, session: &fakeSession{}, evidence: make(EvidenceIndex)}
			test := TestCase{ID: "T1", Name: "Search", Steps: []string{"Verify result"}, Success: "Result visible", DOM: &DOMPlan{Assertions: []string{"Result visible"}}}
			result, _, complete, err := r.executeDOM(context.Background(), a, test)
			if err != nil || complete != (reply.confidence >= jevMinConfidence) || len(result.Evidence) != 1 {
				t.Fatalf("result/complete/error = %#v/%v/%v", result, complete, err)
			}
			if complete {
				expected := domain.ReportVerdictPassed
				if reply.choice == "not_satisfied" {
					expected = domain.ReportVerdictFailed
				}
				report := normalizeVerdict(CriticResult{Verdict: "passed", Summary: "Reviewed"}, ExecutionResult{Cases: []CaseResult{result}}, a.evidence)
				if report.Verdict != expected {
					t.Fatalf("verdict = %s", report.Verdict)
				}
			}
		})
	}
}

func TestJevSecretBindingsStayLocal(t *testing.T) {
	client, requests := testJev(t, jevReply{"e1-1", .99, 0}, jevReply{"satisfied", .99, 0})
	secrets, _ := newSecretSet(map[string]string{"password": "super-secret"})
	defer secrets.Close()
	p, _ := policy.New("https://example.com", domain.RunAuthorization{AllowMutations: true})
	db, _ := newTestStore(t)
	run := queuedRun(t, db)
	r := New(db, nil, Options{Jev: client, ScreenshotDir: t.TempDir()})
	session := &domSession{fakeSession: &fakeSession{inspectText: "super-secret", elements: map[string]browser.Element{"e1-1": {Mutating: true}}}}
	a := &browserAdapter{runner: r, runID: run.ID, session: session, policy: p, secrets: secrets, evidence: make(EvidenceIndex)}
	test := TestCase{ID: "T1", Name: "Login", Success: "Signed in", DOM: &DOMPlan{Actions: []DOMAction{{Tool: "type_text", Target: "Company search", Secret: "password"}}, Assertions: []string{"Signed in"}}}
	_, _, complete, err := r.executeDOM(context.Background(), a, test)
	if err != nil || !complete || session.typedValue != "super-secret" || !session.typedSensitive {
		t.Fatalf("complete/error/sensitive = %v/%v/%v", complete, err, session.typedSensitive)
	}
	if strings.Contains(strings.Join(*requests, ""), "super-secret") {
		t.Fatal("Jev received a raw secret")
	}
}

func TestJevActivationBudgetAndCancellation(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "")
	if New(nil, nil, Options{}).jev != nil {
		t.Fatal("Jev enabled without a key")
	}
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	if New(nil, nil, Options{}).jev == nil {
		t.Fatal("Jev key did not enable integration")
	}
	client, requests := testJev(t)
	r := New(nil, nil, Options{Jev: client})
	r.jevCalls = maxJevCalls
	_, reliable, err := r.choose(context.Background(), "", "test", "state", "question", map[string]string{"yes": "yes"})
	if err != nil || reliable || len(*requests) != 0 {
		t.Fatal("Jev budget not enforced")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = r.choose(ctx, "", "test", "state", "question", map[string]string{"yes": "yes"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestStructuredDOMPlansAreOptionalAndBounded(t *testing.T) {
	var plan TestPlan
	if err := decodeContract(domSearchPlan, &plan); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Replace(domSearchPlan, `"tool":"click"`, `"tool":"click","ref":"stale"`, 1),
		strings.Replace(domSearchPlan, `"assertions":["Acme is visible in the search result"]`, `"assertions":[]`, 1),
	} {
		if err := decodeContract(invalid, &plan); err == nil {
			t.Fatal("accepted malformed structured plan")
		}
	}
}
