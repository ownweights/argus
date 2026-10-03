package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
	"github.com/ace-foundry/argus-testing/argus/internal/domain"
)

func (r *Runner) completeContract(
	ctx context.Context,
	spec agent.AgentSpec,
	sessionID string,
	message *agent.Message,
	runID string,
	target any,
	additionalValidation func() error,
) (string, error) {
	next := message
	for attempt := 0; attempt < 2; attempt++ {
		output, err := r.runAgent(ctx, spec, sessionID, next, runID)
		if err != nil {
			return "", err
		}
		contractError := decodeContract(output, target)
		if contractError == nil && additionalValidation != nil {
			contractError = additionalValidation()
		}
		if contractError == nil {
			encoded, err := json.Marshal(target)
			if err != nil {
				return "", fmt.Errorf("marshal validated contract: %w", err)
			}
			return string(encoded), nil
		}
		if attempt == 1 {
			return "", fmt.Errorf("%w after repair: %v", errInvalidStageContract, contractError)
		}
		resetContract(target)
		next = textMessage(
			"Your previous response did not satisfy the required JSON contract. " +
				"Correct it and return only the complete JSON object. Validation error: " + contractError.Error(),
		)
	}
	return "", errInvalidStageContract
}

func resetContract(target any) {
	switch value := target.(type) {
	case *TestBrief:
		*value = TestBrief{}
	case *AppMap:
		*value = AppMap{}
	case *TestPlan:
		*value = TestPlan{}
	case *ExecutionResult:
		*value = ExecutionResult{}
	case *CriticResult:
		*value = CriticResult{}
	}
}

func validateExecutionAgainstPlan(plan TestPlan, execution ExecutionResult) error {
	if len(plan.Cases) != len(execution.Cases) {
		return fmt.Errorf("%w: execution case count does not match plan", errInvalidStageContract)
	}
	planned := make(map[string]struct{}, len(plan.Cases))
	for _, test := range plan.Cases {
		planned[test.ID] = struct{}{}
	}
	for _, result := range execution.Cases {
		if _, ok := planned[result.ID]; !ok {
			return fmt.Errorf("%w: execution contains unplanned case %q", errInvalidStageContract, result.ID)
		}
	}
	return nil
}

var errInvalidStageContract = errors.New("invalid stage contract")

type TestBrief struct {
	Objective   string   `json:"objective"`
	Features    []string `json:"features"`
	Constraints []string `json:"constraints"`
}

type AppPage struct {
	URL      string   `json:"url"`
	Name     string   `json:"name"`
	Features []string `json:"features"`
}

type AppMap struct {
	Pages []AppPage `json:"pages"`
}

type TestCase struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Steps   []string `json:"steps"`
	Success string   `json:"success"`
	DOM     *DOMPlan `json:"dom,omitempty"`
}

type DOMPlan struct {
	Actions    []DOMAction `json:"actions"`
	Assertions []string    `json:"assertions"`
}

type DOMAction struct {
	Tool   string  `json:"tool"`
	Target string  `json:"target,omitempty"`
	Text   *string `json:"text,omitempty"`
	Secret string  `json:"secret,omitempty"`
	Value  string  `json:"value,omitempty"`
	URL    string  `json:"url,omitempty"`
}

type TestPlan struct {
	Cases []TestCase `json:"cases"`
}

type CaseResult struct {
	ID       string   `json:"id"`
	Status   string   `json:"status"`
	Steps    []string `json:"steps"`
	Findings []string `json:"findings"`
	Evidence []string `json:"evidence"`
}

type ExecutionResult struct {
	Cases   []CaseResult `json:"cases"`
	Summary string       `json:"summary"`
}

type CriticResult struct {
	Verdict         string           `json:"verdict"`
	Summary         string           `json:"summary"`
	Findings        []domain.Finding `json:"findings"`
	Recommendations []string         `json:"recommendations"`
}

type Evidence struct {
	Kind string
	Path string
}

type EvidenceIndex map[string]Evidence

func decodeContract(value string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(unfence(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %v", errInvalidStageContract, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON", errInvalidStageContract)
	}
	var err error
	switch value := target.(type) {
	case *TestBrief:
		err = validateTestBrief(*value)
	case *AppMap:
		err = validateAppMap(*value)
	case *TestPlan:
		err = validateTestPlan(*value)
	case *ExecutionResult:
		err = validateExecution(*value)
	case *CriticResult:
		err = validateCritic(*value)
	default:
		err = errors.New("unsupported contract type")
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errInvalidStageContract, err)
	}
	return nil
}

func validateTestBrief(value TestBrief) error {
	if !boundedRequired(value.Objective, 2_000) || len(value.Features) > 50 || len(value.Constraints) > 50 {
		return errors.New("brief fields are missing or oversized")
	}
	return validateStrings(value.Features, 1_000)
}

func validateAppMap(value AppMap) error {
	if len(value.Pages) == 0 || len(value.Pages) > 50 {
		return errors.New("app map must contain 1 to 50 pages")
	}
	for _, page := range value.Pages {
		if !boundedRequired(page.URL, 2_000) || !boundedRequired(page.Name, 500) || len(page.Features) > 50 {
			return errors.New("app page is invalid")
		}
		if err := validateStrings(page.Features, 1_000); err != nil {
			return err
		}
	}
	return nil
}

func validateTestPlan(value TestPlan) error {
	if len(value.Cases) == 0 || len(value.Cases) > 50 {
		return errors.New("test plan must contain 1 to 50 cases")
	}
	seen := make(map[string]struct{}, len(value.Cases))
	for _, test := range value.Cases {
		if !boundedRequired(test.ID, 100) || !boundedRequired(test.Name, 500) ||
			!boundedRequired(test.Success, 1_000) || len(test.Steps) == 0 || len(test.Steps) > 50 {
			return errors.New("test case is invalid")
		}
		if _, duplicate := seen[test.ID]; duplicate {
			return errors.New("duplicate test case ID")
		}
		seen[test.ID] = struct{}{}
		if err := validateStrings(test.Steps, 1_000); err != nil {
			return err
		}
		if test.DOM != nil {
			if err := validateDOMPlan(*test.DOM); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateExecution(value ExecutionResult) error {
	if len(value.Cases) == 0 || len(value.Cases) > 50 || len(value.Summary) > maxReportText {
		return errors.New("execution result is empty or oversized")
	}
	seen := make(map[string]struct{}, len(value.Cases))
	for _, result := range value.Cases {
		if !boundedRequired(result.ID, 100) ||
			result.Status != "passed" && result.Status != "failed" && result.Status != "inconclusive" ||
			len(result.Steps) > 100 || len(result.Findings) > 50 || len(result.Evidence) > 50 {
			return errors.New("case result is invalid")
		}
		if _, duplicate := seen[result.ID]; duplicate {
			return errors.New("duplicate case result ID")
		}
		seen[result.ID] = struct{}{}
		for _, values := range [][]string{result.Steps, result.Findings, result.Evidence} {
			if err := validateStrings(values, 2_000); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateCritic(value CriticResult) error {
	if value.Verdict != "passed" && value.Verdict != "failed" && value.Verdict != "inconclusive" ||
		!boundedRequired(value.Summary, maxReportText) || len(value.Findings) > 50 || len(value.Recommendations) > 50 {
		return errors.New("critic result is invalid")
	}
	for _, finding := range value.Findings {
		if !boundedRequired(finding.Severity, 80) || !boundedRequired(finding.Title, 500) || !boundedRequired(finding.Detail, maxReportText) {
			return errors.New("critic finding is invalid")
		}
	}
	return validateStrings(value.Recommendations, 1_000)
}

func validateStrings(values []string, maximum int) error {
	for _, value := range values {
		if !boundedRequired(value, maximum) {
			return errors.New("string list contains an empty or oversized value")
		}
	}
	return nil
}

func boundedRequired(value string, maximum int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len([]rune(value)) <= maximum
}

func evidenceVerdict(execution ExecutionResult, evidence EvidenceIndex) domain.ReportVerdict {
	if len(execution.Cases) == 0 {
		return domain.ReportVerdictInconclusive
	}
	for _, result := range execution.Cases {
		if result.Status == "failed" {
			return domain.ReportVerdictFailed
		}
	}
	for _, result := range execution.Cases {
		if result.Status != "passed" || len(result.Evidence) == 0 {
			return domain.ReportVerdictInconclusive
		}
		for _, reference := range result.Evidence {
			item, ok := evidence[reference]
			if !ok || item.Path != reference || item.Kind == "" {
				return domain.ReportVerdictInconclusive
			}
		}
	}
	return domain.ReportVerdictPassed
}

func normalizeVerdict(critic CriticResult, execution ExecutionResult, evidence EvidenceIndex) *domain.RunReport {
	evidenceResult := evidenceVerdict(execution, evidence)
	verdict := evidenceResult
	if verdict != domain.ReportVerdictFailed {
		switch critic.Verdict {
		case "failed":
			verdict = domain.ReportVerdictFailed
		case "inconclusive":
			verdict = domain.ReportVerdictInconclusive
		case "passed":
			// The deterministic evidence verdict remains authoritative.
		default:
			verdict = domain.ReportVerdictInconclusive
		}
	}
	findings := append([]domain.Finding{}, critic.Findings...)
	recommendations := append([]string{}, critic.Recommendations...)
	if critic.Verdict == "passed" && evidenceResult == domain.ReportVerdictInconclusive {
		findings = append(findings, domain.Finding{
			Severity: "warning",
			Title:    "Insufficient positive evidence",
			Detail:   "At least one passed test case did not cite a persisted screenshot from this run.",
		})
	}
	return &domain.RunReport{
		Verdict: verdict, Summary: bounded(critic.Summary, maxReportText),
		Findings: findings, Recommendations: recommendations,
	}
}
