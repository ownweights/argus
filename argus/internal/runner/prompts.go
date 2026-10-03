package runner

import (
	"encoding/json"

	"github.com/ace-foundry/argus-testing/argus/internal/agent"
)

// These are the public QA prompt contracts. They deliberately prohibit handling
// credentials and destructive actions even when supplied in user instructions.
const validatorInstruction = `You are a Request Validator for a web-app QA testing agent. Decide whether the request is to test a web application. Greetings, small talk, general questions, off-topic or meaningless requests are not testable. A URL alone is testable and borderline actionable requests are testable. Respond ONLY with JSON: {"testable":true,"reason":"short reason"}.`

const comprehenderInstruction = `You are a QA Intent Comprehender. Return ONLY JSON matching exactly: {"objective":"...","features":["..."],"constraints":["..."]}. Capture explicit features, navigation needs, scope, implicit verification requirements, and every hard user constraint. Never repeat or request credentials, passwords, API keys, tokens, or other secret values. Authenticated testing may use only named secret bindings explicitly listed by the runner.`

const explorerInstruction = `You are a QA App Explorer. Use inspect_page before actions and return ONLY JSON matching exactly: {"pages":[{"url":"https://...","name":"...","features":["..."]}]}. Map relevant pages, forms, features, navigation, and feedback patterns. Budget at most 8 interactions/navigation actions. Do not submit forms or perform destructive actions. Respect all hard user constraints.`

const strategistInstruction = `You are a QA Test Strategist. Return ONLY JSON matching exactly: {"cases":[{"id":"T1","name":"...","steps":["..."],"success":"..."}]}. Ground every case in the app map, cover requested features, and carry every applicable user constraint as a hard requirement. Plan state-changing or destructive actions only when the runner advertises the required authority. Refer to secrets only by an available binding name and never include secret values.`

const domPlanInstruction = `For cases whose entire success criteria can be established from visible DOM text and element metadata, add an optional "dom" object alongside the existing fields: {"actions":[{"tool":"type_text","target":"Company search","text":"Acme"},{"tool":"click","target":"Show result"}],"assertions":["Acme appears in the search result"]}. Supported tools are click, type_text, select_option, submit_form, and navigate. Use a semantic target description, never an element reference or CSS selector. type_text requires either text or a named secret binding; select_option requires value; navigate requires an absolute url instead of target. Empty actions are allowed for inspection-only cases. Assertions must cover the complete success criterion and all applicable constraints, not just a convenient subset. Omit dom for visual/layout checks, hidden backend state, complex branching, unsupported actions, or uncertain plans. Preserve the normal steps and success fields for fallback.`

const executorInstruction = `You are a QA Test Executor. Execute every planned case and return ONLY JSON matching exactly: {"cases":[{"id":"T1","status":"passed|failed|inconclusive","steps":["..."],"findings":["..."],"evidence":["/screenshots/...png"]}],"summary":"..."}. Use exactly the planned case IDs. Inspect before using an element reference. State-changing and destructive actions are enforced by the runner's explicit policy. Use only named secret bindings, never raw secret values. Every passed case must cite at least one screenshot path returned by a tool. No error alone is inconclusive.`

const criticInstruction = `You are an independent QA Critic. Review the request, brief, app map, plan, execution, and evidence. Use inspect_page and screenshot if needed. Return ONLY JSON matching exactly: {"verdict":"passed|failed|inconclusive","summary":"...","findings":[{"severity":"...","title":"...","detail":"..."}],"recommendations":["..."]}. Passed requires every requested feature to be meaningfully tested with cited evidence; failed requires a demonstrated failure; otherwise use inconclusive. Never expose credentials, secrets, or unnecessary page contents.`

func spec(name, instruction string, model agent.ModelRef, tools []agent.Tool, jsonMode bool) agent.AgentSpec {
	return agent.AgentSpec{Name: name, Model: model, Instruction: instruction, Tools: tools, Generation: agent.GenerationOptions{JSONMode: jsonMode}}
}

func browserTools(adapter *browserAdapter, includeActions bool) []agent.Tool {
	inspect := agent.Tool{Name: "inspect_page", Description: "Inspect the current page URL, title, visible text, and interactive elements.", InputSchema: schema(`{"type":"object","properties":{},"additionalProperties":false}`), Invoke: adapter.inspect}
	screenshot := agent.Tool{Name: "screenshot", Description: "Capture a screenshot for evidence. Use after significant actions.", InputSchema: schema(`{"type":"object","properties":{"label":{"type":"string","maxLength":80}},"additionalProperties":false}`), Invoke: adapter.screenshot}
	tools := []agent.Tool{inspect}
	groundingAvailable := adapter != nil && adapter.runner != nil && adapter.runner.grounder != nil
	if groundingAvailable {
		tools = append(tools, agent.Tool{Name: "find_elements", Description: "Locate visible UI targets in a fresh viewport screenshot.", InputSchema: schema(`{"type":"object","properties":{"description":{"type":"string","minLength":1,"maxLength":500},"limit":{"type":"integer","minimum":1,"maximum":10}},"required":["description"],"additionalProperties":false}`), Invoke: adapter.findElements})
	}
	if !includeActions {
		return append(tools, screenshot)
	}
	if groundingAvailable {
		tools = append(tools, agent.Tool{Name: "visual_click", Description: "Locate a target in a fresh screenshot, enforce policy on the DOM element at that point, and click it.", InputSchema: schema(`{"type":"object","properties":{"description":{"type":"string","minLength":1,"maxLength":500}},"required":["description"],"additionalProperties":false}`), Invoke: adapter.visualClick})
	}
	return append(tools, []agent.Tool{
		{Name: "click", Description: "Click one inspected element by its current reference.", InputSchema: schema(`{"type":"object","properties":{"ref":{"type":"string","minLength":1,"maxLength":100}},"required":["ref"],"additionalProperties":false}`), Invoke: adapter.click},
		{Name: "type_text", Description: "Replace an inspected field value with plain text or a named ephemeral secret binding.", InputSchema: schema(`{"type":"object","properties":{"ref":{"type":"string","minLength":1,"maxLength":100},"text":{"type":"string","maxLength":4096},"secret":{"type":"string","pattern":"^[A-Za-z_][A-Za-z0-9_.-]{0,99}$"}},"required":["ref"],"oneOf":[{"required":["text"],"not":{"required":["secret"]}},{"required":["secret"],"not":{"required":["text"]}}],"additionalProperties":false}`), Invoke: adapter.typeText},
		{Name: "fill_form", Description: "Fill up to 20 inspected fields using plain text or named ephemeral secret bindings.", InputSchema: schema(`{"type":"object","properties":{"fields":{"type":"array","minItems":1,"maxItems":20,"items":{"type":"object","properties":{"ref":{"type":"string","minLength":1,"maxLength":100},"text":{"type":"string","maxLength":4096},"secret":{"type":"string","pattern":"^[A-Za-z_][A-Za-z0-9_.-]{0,99}$"}},"required":["ref"],"oneOf":[{"required":["text"],"not":{"required":["secret"]}},{"required":["secret"],"not":{"required":["text"]}}],"additionalProperties":false}}},"required":["fields"],"additionalProperties":false}`), Invoke: adapter.fillForm},
		{Name: "submit_form", Description: "Submit an inspected form control. This always requires mutation authority.", InputSchema: schema(`{"type":"object","properties":{"ref":{"type":"string","minLength":1,"maxLength":100}},"required":["ref"],"additionalProperties":false}`), Invoke: adapter.submitForm},
		{Name: "select_option", Description: "Select an option by value or visible label on an inspected select element.", InputSchema: schema(`{"type":"object","properties":{"ref":{"type":"string","minLength":1,"maxLength":100},"value":{"type":"string","minLength":1,"maxLength":1000}},"required":["ref","value"],"additionalProperties":false}`), Invoke: adapter.selectOption},
		{Name: "press_key", Description: "Press a Playwright keyboard key. Activating keys require mutation authority.", InputSchema: schema(`{"type":"object","properties":{"key":{"type":"string","minLength":1,"maxLength":100}},"required":["key"],"additionalProperties":false}`), Invoke: adapter.pressKey},
		{Name: "scroll", Description: "Scroll the current page vertically by a bounded pixel delta.", InputSchema: schema(`{"type":"object","properties":{"delta_y":{"type":"integer","minimum":-10000,"maximum":10000}},"required":["delta_y"],"additionalProperties":false}`), Invoke: adapter.scroll},
		{Name: "resize_viewport", Description: "Resize the browser viewport for responsive testing.", InputSchema: schema(`{"type":"object","properties":{"width":{"type":"integer","minimum":240,"maximum":3840},"height":{"type":"integer","minimum":240,"maximum":3840}},"required":["width","height"],"additionalProperties":false}`), Invoke: adapter.resize},
		{Name: "wait_for", Description: "Wait for visible text, a URL fragment, or both.", InputSchema: schema(`{"type":"object","properties":{"text":{"type":"string","maxLength":1000},"url":{"type":"string","maxLength":2000},"timeout_ms":{"type":"integer","minimum":1,"maximum":30000}},"anyOf":[{"required":["text"]},{"required":["url"]}],"additionalProperties":false}`), Invoke: adapter.waitFor},
		{Name: "console_errors", Description: "Return bounded browser console errors captured during this run.", InputSchema: schema(`{"type":"object","properties":{},"additionalProperties":false}`), Invoke: adapter.consoleErrors},
		{Name: "network_errors", Description: "Return bounded failed and HTTP error requests without headers or bodies.", InputSchema: schema(`{"type":"object","properties":{},"additionalProperties":false}`), Invoke: adapter.networkErrors},
		{Name: "navigate", Description: "Navigate to an HTTP(S) URL without credentials.", InputSchema: schema(`{"type":"object","properties":{"url":{"type":"string","minLength":1,"maxLength":2000}},"required":["url"],"additionalProperties":false}`), Invoke: adapter.navigate},
		screenshot,
	}...)
}

func schema(value string) json.RawMessage { return json.RawMessage(value) }
