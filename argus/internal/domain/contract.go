package domain

const (
	RunsEndpoint      = "/api/runs"
	RunEndpoint       = "/api/runs/{run_id}"
	CancelRunEndpoint = "/api/runs/{run_id}/cancel"
	SettingsEndpoint  = "/api/settings"
	RunEventsEndpoint = "/ws/runs/{run_id}"
)

type ProviderID string

const (
	ProviderGemini ProviderID = "gemini"
	ProviderGPT    ProviderID = "gpt"
	ProviderKimi   ProviderID = "kimi"
)

func ValidProvider(provider ProviderID) bool {
	return provider == ProviderGemini || provider == ProviderGPT || provider == ProviderKimi
}

type RunStatus string

const (
	RunStatusQueued    RunStatus = "queued"
	RunStatusRunning   RunStatus = "running"
	RunStatusPassed    RunStatus = "passed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusCancelled RunStatus = "cancelled"
)

type ReportVerdict string

const (
	ReportVerdictPassed       ReportVerdict = "passed"
	ReportVerdictFailed       ReportVerdict = "failed"
	ReportVerdictInconclusive ReportVerdict = "inconclusive"
)

type EventType string

const (
	EventRunQueued          EventType = "run.queued"
	EventRunStarted         EventType = "run.started"
	EventRunCompleted       EventType = "run.completed"
	EventRunFailed          EventType = "run.failed"
	EventRunCancelled       EventType = "run.cancelled"
	EventPlanCompleted      EventType = "plan.completed"
	EventBrowserAction      EventType = "browser.action"
	EventBrowserObservation EventType = "browser.observation"
	EventBrowserScreenshot  EventType = "browser.screenshot"
)

type Finding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type RunReport struct {
	Verdict         ReportVerdict `json:"verdict"`
	Summary         string        `json:"summary"`
	Plan            *string       `json:"plan,omitempty"`
	Findings        []Finding     `json:"findings"`
	Recommendations []string      `json:"recommendations"`
}

type RunEvent struct {
	ID        int64          `json:"id"`
	RunID     string         `json:"run_id"`
	Type      EventType      `json:"type"`
	Data      map[string]any `json:"data"`
	CreatedAt string         `json:"created_at"`
}

type Run struct {
	ID           string     `json:"id"`
	URL          string     `json:"url"`
	Instructions string     `json:"instructions"`
	Provider     ProviderID `json:"provider"`
	Status       RunStatus  `json:"status"`
	CreatedAt    string     `json:"created_at"`
	UpdatedAt    string     `json:"updated_at"`
	Error        *string    `json:"error"`
	Report       *RunReport `json:"report"`
	Policy       *RunPolicy `json:"policy,omitempty"`
	Events       []RunEvent `json:"events,omitempty"`
}

type CreateRequest struct {
	URL           string            `json:"url"`
	Instructions  string            `json:"instructions"`
	Provider      ProviderID        `json:"provider,omitempty"`
	Authorization *RunAuthorization `json:"authorization,omitempty"`
}

// RunAuthorization contains caller-provided execution authority. SecretBindings
// are ephemeral and must never be copied into Run or persisted by the store.
type RunAuthorization struct {
	AllowMutations   bool              `json:"allow_mutations,omitempty"`
	AllowDestructive bool              `json:"allow_destructive,omitempty"`
	AllowedOrigins   []string          `json:"allowed_origins,omitempty"`
	SecretBindings   map[string]string `json:"secret_bindings,omitempty"`
}

// RunPolicy is the non-secret portion of RunAuthorization that may be persisted.
type RunPolicy struct {
	AllowMutations   bool     `json:"allow_mutations"`
	AllowDestructive bool     `json:"allow_destructive"`
	AllowedOrigins   []string `json:"allowed_origins"`
}

type ProviderInfo struct {
	ID        ProviderID `json:"id"`
	Available bool       `json:"available"`
	Default   bool       `json:"default"`
}

type SettingsResponse struct {
	GeminiConfigured bool           `json:"gemini_configured"`
	Model            string         `json:"model"` // Legacy compatibility field; provider catalog never exposes models.
	Providers        []ProviderInfo `json:"providers"`
}
