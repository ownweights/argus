// Package runner connects run state to the public QA pipeline.
package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/browser"
	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	"github.com/ace-foundry/argus-testing/argus/internal/gemini"
	"github.com/ace-foundry/argus-testing/argus/internal/openai"
	"github.com/ace-foundry/argus-testing/argus/internal/policy"
	"github.com/ace-foundry/argus-testing/argus/internal/store"
)

const (
	missingAPIKey    = "GEMINI_API_KEY is not configured"
	missingOpenAIKey = "OPENAI_API_KEY is not configured"
	missingKimiKey   = "KIMI_API_KEY is not configured"
	defaultGPTModel  = "gpt-4o"
	defaultKimiModel = "moonshot-v1-8k-vision-preview"
	defaultKimiURL   = "https://api.moonshot.ai/v1"
)

type Publisher func(domain.RunEvent)

type Options struct {
	ScreenshotDir string
	Timeout       time.Duration
	Model         string // Gemini model, retained for compatibility.
	APIKey        string // Gemini API key, retained for compatibility.
	GPTModel      string
	GPTAPIKey     string
	KimiModel     string
	KimiAPIKey    string
	KimiBaseURL   string
	Provider      agent.Provider // Provider makes deterministic Gemini tests possible.
	GPTProvider   agent.Provider
	KimiProvider  agent.Provider
	Grounder      Grounder
}

type Runner struct {
	store         *store.Store
	browser       browser.Factory
	screenshotDir string
	timeout       time.Duration
	model         agent.ModelRef
	models        map[domain.ProviderID]agent.ModelRef
	runtime       *agent.Runtime
	grounder      Grounder
	grounders     map[domain.ProviderID]Grounder
	configured    map[domain.ProviderID]bool
	publish       Publisher
}

func New(runStore *store.Store, factory browser.Factory, options Options) *Runner {
	if factory == nil {
		factory = browser.NewPlaywrightFactory()
	}
	if options.Timeout <= 0 {
		options.Timeout = timeoutFromEnv()
	}
	if options.Model == "" {
		options.Model = "gemini-2.5-flash"
	}
	if options.GPTModel == "" {
		options.GPTModel = defaultGPTModel
	}
	if options.KimiModel == "" {
		options.KimiModel = defaultKimiModel
	}
	if options.KimiBaseURL == "" {
		options.KimiBaseURL = defaultKimiURL
	}
	if options.APIKey == "" {
		options.APIKey = os.Getenv("GEMINI_API_KEY")
	}
	if options.GPTAPIKey == "" {
		options.GPTAPIKey = os.Getenv("OPENAI_API_KEY")
	}
	if options.KimiAPIKey == "" {
		options.KimiAPIKey = os.Getenv("KIMI_API_KEY")
	}
	if baseURL := os.Getenv("KIMI_BASE_URL"); options.KimiBaseURL == defaultKimiURL && baseURL != "" {
		options.KimiBaseURL = baseURL
	}

	geminiProvider := options.Provider
	if geminiProvider == nil && options.APIKey != "" {
		geminiProvider = gemini.New(options.APIKey)
	}
	gptProvider := options.GPTProvider
	if gptProvider == nil && options.GPTAPIKey != "" {
		gptProvider = openai.New(options.GPTAPIKey)
	}
	kimiProvider := options.KimiProvider
	if kimiProvider == nil && options.KimiAPIKey != "" {
		kimiProvider = openai.New(options.KimiAPIKey, openai.WithBaseURL(options.KimiBaseURL))
	}
	providers := map[string]agent.Provider{}
	configured := map[domain.ProviderID]bool{}
	for _, provider := range []struct {
		id       domain.ProviderID
		provider agent.Provider
	}{{domain.ProviderGemini, geminiProvider}, {domain.ProviderGPT, gptProvider}, {domain.ProviderKimi, kimiProvider}} {
		if provider.provider != nil {
			providers[string(provider.id)] = provider.provider
			configured[provider.id] = true
		}
	}
	grounder := options.Grounder
	if grounder == nil {
		if direct, ok := geminiProvider.(Grounder); ok {
			grounder = direct
		} else if provider, ok := geminiProvider.(*gemini.Provider); ok {
			grounder = geminiGrounder{provider: provider, model: options.Model}
		}
	}
	grounders := map[domain.ProviderID]Grounder{}
	if grounder != nil {
		grounders[domain.ProviderGemini] = grounder
	}
	var runtime *agent.Runtime
	if len(providers) > 0 {
		runtime = agent.NewRuntime(providers, agent.NewInMemorySessionStore(), agent.WithMaxModelCalls(32))
	}
	return &Runner{
		store: runStore, browser: factory, screenshotDir: options.ScreenshotDir, timeout: options.Timeout,
		model: agent.ModelRef{Provider: string(domain.ProviderGemini), Model: options.Model},
		models: map[domain.ProviderID]agent.ModelRef{
			domain.ProviderGemini: {Provider: string(domain.ProviderGemini), Model: options.Model},
			domain.ProviderGPT:    {Provider: string(domain.ProviderGPT), Model: options.GPTModel},
			domain.ProviderKimi:   {Provider: string(domain.ProviderKimi), Model: options.KimiModel},
		},
		runtime: runtime, grounder: grounder, grounders: grounders, configured: configured,
	}
}

func (r *Runner) configFor(provider domain.ProviderID) (agent.ModelRef, Grounder, string) {
	model := r.models[provider]
	if r.configured[provider] {
		return model, r.grounders[provider], ""
	}
	switch provider {
	case domain.ProviderGemini:
		return model, nil, missingAPIKey
	case domain.ProviderGPT:
		return model, nil, missingOpenAIKey
	case domain.ProviderKimi:
		return model, nil, missingKimiKey
	default:
		return model, nil, "Provider is not configured"
	}
}

func timeoutFromEnv() time.Duration {
	if seconds, err := strconv.Atoi(os.Getenv("ARGUS_RUN_TIMEOUT")); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 5 * time.Minute
}

func (r *Runner) SetPublisher(publish Publisher) { r.publish = publish }

// Run executes one queued run. Store writes always complete before publication.
func (r *Runner) Run(parent context.Context, id string, authorization domain.RunAuthorization) {
	if r.store == nil {
		return
	}
	run, err := r.store.GetRun(id, false)
	if err != nil || run == nil || parent.Err() != nil {
		return
	}
	model, grounder, missing := r.configFor(run.Provider)
	if missing != "" {
		r.fail(id, missing, "configuration")
		return
	}
	execution := *r
	execution.model = model
	execution.grounder = grounder
	ctx, cancel := context.WithTimeout(parent, r.timeout)
	defer cancel()
	started, err := r.store.Transition(id, []domain.RunStatus{domain.RunStatusQueued}, domain.RunStatusRunning, domain.EventRunStarted, nil, nil, nil)
	if err != nil || started == nil {
		return
	}
	r.publishEvent(*started)

	report, err := execution.execute(ctx, id, run, authorization)
	if parent.Err() != nil { // The server owns cancellation and has already recorded it.
		return
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.fail(id, "Run timed out", "timeout")
		}
		return
	}
	if err != nil {
		r.fail(id, publicError(err), errorKind(err))
		return
	}
	status := domain.RunStatusPassed
	if report.Verdict != domain.ReportVerdictPassed {
		status = domain.RunStatusFailed
	}
	event, err := r.store.Transition(id, []domain.RunStatus{domain.RunStatusRunning}, status, domain.EventRunCompleted, map[string]any{"verdict": report.Verdict}, report, nil)
	if err == nil && event != nil {
		r.publishEvent(*event)
	}
}

func (r *Runner) execute(ctx context.Context, id string, run *domain.Run, authorization domain.RunAuthorization) (*domain.RunReport, error) {
	effectiveAuthorization := authorization
	if run.Policy != nil {
		effectiveAuthorization.AllowMutations = run.Policy.AllowMutations
		effectiveAuthorization.AllowDestructive = run.Policy.AllowDestructive
		effectiveAuthorization.AllowedOrigins = append([]string(nil), run.Policy.AllowedOrigins...)
	}
	runPolicy, err := policy.New(run.URL, effectiveAuthorization)
	if err != nil {
		return nil, fmt.Errorf("run policy: %w", err)
	}
	secrets, err := newSecretSet(authorization.SecretBindings)
	if err != nil {
		return nil, err
	}
	defer secrets.Close()

	validator, err := r.complete(ctx, spec("validator", validatorInstruction, r.model, nil, true), id+":validator", runMessage(run))
	if err != nil {
		return nil, err
	}
	if testable, reason := parseValidator(validator); !testable {
		return normalizedReport("", "Validation failed — request is not testable", "", reason), nil
	}
	var briefContract TestBrief
	brief, err := r.completeContract(
		ctx, spec("comprehender", comprehenderInstruction, r.model, nil, true),
		id+":comprehender", runMessage(run), "", &briefContract, nil,
	)
	if err != nil {
		return nil, err
	}

	session, err := r.browser.Open(ctx, browser.SessionOptions{
		AllowMutations: effectiveAuthorization.AllowMutations,
		AllowNavigation: func(target string) bool {
			return runPolicy.CheckNavigation(target) == nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("browser: %w", err)
	}
	defer session.Close()
	if _, err := session.Navigate(ctx, run.URL); err != nil {
		return nil, fmt.Errorf("browser: %w", err)
	}
	adapter := &browserAdapter{
		runner: r, runID: id, session: session, policy: runPolicy, secrets: secrets,
		evidence: make(EvidenceIndex),
	}
	initial, err := adapter.capture(ctx, "Initial page", true)
	if err != nil {
		return nil, err
	}

	var appMapContract AppMap
	explorer, err := r.completeContract(
		ctx, spec("explorer", explorerInstruction, r.model, browserTools(adapter, true), true),
		id+":explorer", imageMessage("Target: "+run.URL+"\nGoal: "+run.Instructions+"\nTest Brief:\n"+brief, initial.data),
		id, &appMapContract, nil,
	)
	if err != nil {
		return nil, err
	}
	var planContract TestPlan
	plan, err := r.completeContract(
		ctx, spec("strategist", strategistInstruction, r.model, nil, true),
		id+":strategist", textMessage("Target: "+run.URL+"\nGoal: "+run.Instructions+"\nTest Brief:\n"+brief+"\nApp Map:\n"+explorer),
		"", &planContract, nil,
	)
	if err != nil {
		return nil, err
	}
	if err := r.addEvent(id, domain.EventPlanCompleted, map[string]any{"plan": bounded(plan, 12000)}); err != nil {
		return nil, err
	}
	preExecution, err := adapter.capture(ctx, "Pre-execution", false)
	if err != nil {
		return nil, err
	}
	secretBindings := secrets.Names()
	secretContext := ""
	if len(secretBindings) > 0 {
		secretContext = "\nAvailable secret bindings (names only): " + strings.Join(secretBindings, ", ")
	}
	var executionContract ExecutionResult
	execution, err := r.completeContract(
		ctx, spec("executor", executorInstruction, r.model, browserTools(adapter, true), true),
		id+":executor", imageMessage("Target: "+run.URL+"\nGoal: "+run.Instructions+"\nPlan:\n"+plan+"\nApp Map:\n"+explorer+secretContext, preExecution.data),
		id, &executionContract, func() error {
			return validateExecutionAgainstPlan(planContract, executionContract)
		},
	)
	if err != nil {
		return nil, err
	}
	final, err := adapter.capture(ctx, "Final page", true)
	if err != nil {
		return nil, err
	}
	var criticContract CriticResult
	_, err = r.completeContract(
		ctx, spec("critic", criticInstruction, r.model, browserTools(adapter, false), true),
		id+":critic", imageMessage("Original User Request: "+run.Instructions+"\nTest Brief: "+brief+"\nApp Map: "+explorer+"\nTest Plan: "+plan+"\nExecutor Results: "+execution, final.data),
		id, &criticContract, nil,
	)
	if err != nil {
		return nil, err
	}
	report := normalizeVerdict(criticContract, executionContract, adapter.evidence)
	report.Plan = stringPointer(bounded(plan, 12_000))
	return report, nil
}

func runMessage(run *domain.Run) *agent.Message {
	return textMessage("Target: " + run.URL + "\nGoal: " + run.Instructions)
}
func textMessage(value string) *agent.Message {
	return &agent.Message{Role: agent.RoleUser, Parts: []agent.MessagePart{{Text: &agent.TextPart{Text: value}}}}
}
func imageMessage(value string, image []byte) *agent.Message {
	return &agent.Message{Role: agent.RoleUser, Parts: []agent.MessagePart{{Text: &agent.TextPart{Text: value}}, {Image: &agent.ImagePart{Data: image, MediaType: "image/png"}}}}
}

func (r *Runner) complete(ctx context.Context, agentSpec agent.AgentSpec, sessionID string, message *agent.Message) (string, error) {
	return r.runAgent(ctx, agentSpec, sessionID, message, "")
}

func (r *Runner) runAgent(ctx context.Context, agentSpec agent.AgentSpec, sessionID string, message *agent.Message, runID string) (string, error) {
	var output strings.Builder
	err := r.runtime.Run(ctx, agentSpec, sessionID, message, map[string]any{"run_id": runID}, func(event agent.RuntimeEvent) error {
		switch value := event.(type) {
		case agent.ToolCallEvent:
			if runID != "" {
				for _, tool := range agentSpec.Tools {
					if tool.Name == value.Call.Name {
						return r.addEvent(runID, domain.EventBrowserAction, browser.FormatAction(value.Call.Name, value.Call.Arguments))
					}
				}
			}
		case agent.ToolResultEvent:
			if runID != "" {
				return r.addEvent(runID, domain.EventBrowserObservation, browser.FormatObservation(value.Result.Name, value.Result.Result))
			}
		case agent.CompletedEvent:
			for _, part := range value.Message.Parts {
				if part.Text != nil {
					output.WriteString(part.Text.Text)
				}
			}
		}
		return nil
	})
	return output.String(), err
}

func (r *Runner) addEvent(id string, eventType domain.EventType, data map[string]any) error {
	event, err := r.store.AddEvent(id, eventType, data)
	if err == nil && event != nil {
		r.publishEvent(*event)
	}
	return err
}

func (r *Runner) fail(id, message, kind string) {
	event, err := r.store.Transition(id, []domain.RunStatus{domain.RunStatusQueued, domain.RunStatusRunning}, domain.RunStatusFailed, domain.EventRunFailed, map[string]any{"kind": kind, "message": message}, nil, &message)
	if err == nil && event != nil {
		r.publishEvent(*event)
	}
}
func (r *Runner) publishEvent(event domain.RunEvent) {
	if r.publish != nil {
		r.publish(event)
	}
}

func publicError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "Run timed out"
	}
	var geminiRate *gemini.RateLimitError
	var geminiHTTP *gemini.HTTPError
	var openAIRate *openai.RateLimitError
	var openAIHTTP *openai.HTTPError
	switch {
	case errors.As(err, &geminiRate):
		return "Gemini rate limit exceeded"
	case errors.As(err, &geminiHTTP):
		return "Gemini provider request failed"
	case errors.As(err, &openAIRate):
		return "OpenAI-compatible provider rate limit exceeded"
	case errors.As(err, &openAIHTTP):
		return "OpenAI-compatible provider request failed"
	}
	if strings.HasPrefix(err.Error(), "browser:") {
		return "Browser operation failed"
	}
	return "Pipeline execution failed"
}
func errorKind(err error) string {
	var geminiRate *gemini.RateLimitError
	var geminiHTTP *gemini.HTTPError
	var openAIRate *openai.RateLimitError
	var openAIHTTP *openai.HTTPError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.As(err, &geminiRate), errors.As(err, &openAIRate):
		return "rate_limit"
	case errors.As(err, &geminiHTTP), errors.As(err, &openAIHTTP):
		return "provider"
	case strings.HasPrefix(err.Error(), "browser:"):
		return "browser"
	default:
		return "execution"
	}
}
