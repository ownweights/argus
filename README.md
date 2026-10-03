# Argus

**AI agents that test your UI like a real user — no scripts to write, no selectors to maintain.**

![Website](./frontend/public/landing.png)

Argus is a visual UI testing agent. Describe a test, point it at an HTTP(S) page, and watch it inspect the page in an isolated Playwright browser context. Runs, timelines, screenshot references, and structured reports are captured automatically.

> **Argus finds the bugs you didn't write tests for.** Point it at a page, describe what "working" looks like, and an autonomous agent explores your UI the way a real user would — clicking, typing, scrolling — then hands you a structured report with screenshots and a timeline. No test scripts to maintain, no flaky selectors to babysit.

![Argus dashboard](./frontend/public/dashboard.png)

- **Agentic, not scripted** — the agent reasons about the page and adapts, it doesn't replay a fixed script
- **Fits your stack** — built on Playwright, works against localhost and private-network apps
- **Zero setup ceremony** — start testing in minutes

## Use the hosted platform

The fastest way to run Argus: no install, no API keys to manage, nothing to self-host.

Get started at [argustest.com](https://argustest.com/).

## Six-stage agent pipeline

A run is a bounded six-stage pipeline rather than one unconstrained model call:

1. **Validator** — checks the target URL and test description are actually testable before a run starts.
2. **Comprehender** — reads the test description and breaks it into distinct test cases.
3. **Explorer** — crawls the app first, mapping out pages and the actions available on each.
4. **Strategist** — turns the map and test cases into a concrete step-by-step plan.
5. **Executor** — runs the plan in a real browser: navigating, typing, clicking, and confirming outcomes as it goes.
6. **Critic** — independently reviews the request, execution, and evidence before a verdict is accepted.

Stage handoffs use validated JSON contracts. A malformed handoff gets one repair attempt. A pass is accepted only when every executed case passes and cites a screenshot that was actually persisted for the run.

### Browser capabilities

The Go agent exposes a semantic, reference-based browser surface:

- inspection, navigation, clicking, typing, multi-field filling, form submission, and select controls
- keyboard input, scrolling, viewport resizing, bounded waits, console errors, and network errors
- full-page evidence screenshots and fresh viewport screenshots returned directly to the selected model provider
- visual `find_elements` and guarded `visual_click` grounding when Gemini is configured

Model calls never receive arbitrary CSS-selector access. Each inspection creates run-local element references; stale references are rejected after navigation or a fresh inspection. Visual clicks resolve the real DOM element under Gemini's proposed coordinate and apply the same action policy before clicking.

## Inside Argus

![Describing a test](./frontend/public/auto-test.png)

![A run in progress](./frontend/public/running.png)

![A completed run report](./frontend/public/completed.png)

![Integrations](./frontend/public/integrations.png)

## Example

A test is just a description of intent — Argus figures out how to interact with the page. For example, pointed at Y Combinator's site:

```
Target: https://www.ycombinator.com

Test the startup directory.

1. From the homepage, navigate to the companies/startup directory.
2. Search for a well-known YC company by name (e.g. "Airbnb") and confirm it appears in the results.
3. Filter the directory by a specific batch (e.g. "Winter 2024") and confirm the listed companies update to match.
4. Open a company's profile from the results and confirm its name, one-line description, batch, and website link all render correctly.
5. Navigate back to the directory and confirm the search/filter state behaves as expected — either preserved or reset, whichever the page is designed to do.
6. Resize the viewport to 375px width and confirm the nav collapses into a mobile menu, and the directory list stays scrollable and usable with no overlapping elements.

Fail the test if the known company doesn't appear in search results, if the batch filter doesn't actually filter the list, if a company profile is missing expected fields, or if the mobile layout breaks.
```

Argus runs this like a person would — clicking through the flow, reading the page to judge success or failure — and returns a timeline, screenshots at each step, and a pass/fail report with the reasoning behind it.

## Run it yourself

Argus is source-available and local-first — SQLite storage and no application telemetry. Browser observations and screenshots are sent to the selected configured provider when needed for agent execution; run metadata and evidence remain in the local SQLite/data directory.

### Requirements

- Go 1.25+, Node.js 20.19+ or 22.12+, and a Gemini, OpenAI, or Kimi API key for real runs

### Run locally

From the repository root:

```bash
cd frontend && npm ci && npm run build && cd ..
go -C argus run ./cmd/argus install-browser
GEMINI_API_KEY=your-key GEMINI_MODEL=gemini-2.5-flash ARGUS_RUN_TIMEOUT=300 ARGUS_DB_PATH=data/argus.db PORT=8000 go -C argus run ./cmd/argus
```

Open <http://localhost:8000>. For frontend hot reload, run `npm run dev` in `frontend/` and the Go server in another terminal.

Configuration is environment-only:

| Variable | Default | Purpose |
| --- | --- | --- |
| `GEMINI_API_KEY` | — | Required for real execution |
| `GEMINI_MODEL` | `gemini-2.5-flash` | Gemini REST model |
| `OPENAI_API_KEY` | — | Enables the fixed GPT provider |
| `OPENAI_MODEL` | `gpt-4o` | GPT vision model |
| `KIMI_API_KEY` | — | Enables the fixed Kimi provider |
| `KIMI_MODEL` | `moonshot-v1-8k-vision-preview` | Kimi vision model |
| `KIMI_BASE_URL` | `https://api.moonshot.ai/v1` | Kimi OpenAI-compatible API base URL |
| `ZAI_API_KEY` | — | Enables the fixed GLM-5.3 Flash provider |
| `GLM_MODEL` | `glm-5.3-flash` | GLM model |
| `ZAI_BASE_URL` | `https://api.z.ai/api/paas/v4` | Z.ai OpenAI-compatible API base URL |
| `TYPESAFE_API_KEY` | — | Enables Jev request validation, DOM target selection, and DOM assertions with LLM fallback |
| `JEV_MODEL` | `jev-latest` | TypeSafe decision model |
| `ARGUS_RUN_TIMEOUT` | `300` | Run timeout in seconds |
| `ARGUS_DB_PATH` | `data/argus.db` | SQLite file; screenshots are stored beside it |
| `PORT` | `8000` | Go server port |
| `ARGUS_BASE_URL` | `http://127.0.0.1:8000` | Running local Argus REST server used by the MCP adapter |

When `TYPESAFE_API_KEY` is set, the planner can emit structured DOM actions for suitable cases. Jev selects fresh element references and checks DOM/text outcomes; Go executes actions through the existing policy guards. Decisions below 0.9 confidence, ambiguous results, unsupported cases, or API errors fall back to the selected LLM. Jev calls have a 3-second timeout and a 64-call run budget; an API error disables Jev for the rest of that run. Completed actions are excluded from the fallback plan, and verification-only fallback cannot replay actions. Browser-action errors stop the run rather than retrying a potentially completed mutation. Vision, planning, the independent Critic, and persisted screenshot requirements remain unchanged. The confidence threshold is an initial gate, not a measured accuracy guarantee.

### Run authorization and secret bindings

Runs are read-only by default. The initial target origin is always allowed. Additional origins, state-changing actions, and destructive controls require explicit per-run authorization:

```json
{
  "url": "https://app.example.com",
  "instructions": "Sign in and verify the account page",
  "authorization": {
    "allow_mutations": true,
    "allow_destructive": false,
    "allowed_origins": ["https://accounts.example.com"],
    "secret_bindings": {
      "login_email": "qa@example.com",
      "login_password": "provided-at-request-time"
    }
  }
}
```

Submit that object to `POST /api/runs`. Secret values are copied into run-scoped memory, resolved only at the final typing call, redacted from model-visible text, masked in screenshots, never written to SQLite/events/reports, and zeroed when the run exits. Binding names must match `^[A-Za-z_][A-Za-z0-9_.-]{0,99}$`; at most 20 bindings of 4 KiB each are accepted. The dashboard intentionally exposes policy controls but no raw-secret fields.

Read-only browser contexts abort non-GET/HEAD/OPTIONS requests. Document navigations—including redirects and link clicks—are restricted to the target origin plus `allowed_origins`. `allow_destructive` is invalid unless `allow_mutations` is also enabled.

### Connect an MCP client

Start Argus normally, then install the local stdio adapter from the repository root:

```bash
go install ./argus/cmd/argus-mcp
```

Ensure Go's bin directory (usually `$(go env GOPATH)/bin`) is on your `PATH`, then configure your MCP client:

```json
{
  "mcpServers": {
    "argus": {
      "command": "argus-mcp",
      "env": {"ARGUS_BASE_URL": "http://127.0.0.1:8000"}
    }
  }
}
```

The adapter exposes `start_test`, `get_test_run`, `list_test_runs`, `cancel_test`, and `get_test_evidence`. It connects only to the existing REST server; start Go Argus before using these tools. The same client-specific configuration and server status are available in **Settings → MCP setup**.

Argus accepts normal HTTP(S) targets, including trusted localhost and private-network apps. It rejects credentials and sensitive query parameters in target URLs. The settings screen only shows whether provider configuration is present.

### Docker

```bash
cp .env.example .env
# Set a provider API key in .env
docker compose up --build
```

The UI is available at <http://localhost:8000> and persistent data is written to `./data`.

### Development checks

```bash
cd argus && go test -race ./... && go vet ./...
cd ../frontend && npm ci && npm test && npm run typecheck && npm run lint && npm run build
```

The default Go suite is offline and uses deterministic provider doubles. To exercise the Jev integration against the live API, export `TYPESAFE_API_KEY` and run `ARGUS_PLAYWRIGHT_SMOKE=1 ARGUS_JEV_LIVE=1 go test ./internal/runner -run TestIntegrationJevUsesRealPlaywright -v` from `argus/`. This makes paid Jev calls with synthetic fixture data; the LLM stages remain stubbed. After `go run ./cmd/argus install-browser`, set `ARGUS_PLAYWRIGHT_SMOKE=1` to include the real Chromium fixture and full runner integration:

```bash
cd argus
ARGUS_PLAYWRIGHT_SMOKE=1 go test -race ./...
```

### Architecture

- `argus/cmd/argus`: local REST/WebSocket server and built UI serving
- `argus/internal/runner`: six-stage Gemini pipeline, strict contracts, browser policy, and evidence gating
- `argus/internal/browser`: isolated Playwright sessions, semantic references, request interception, and redacted screenshots
- `argus/cmd/argus-mcp`: local stdio MCP adapter for the REST server
- `frontend`: dashboard/composer, live session, history, report, and read-only settings

There is no server authentication or multi-user isolation in this release. Bind the server to a trusted interface and put an authenticating reverse proxy in front of it when exposing it beyond localhost.

## License

Argus is source-available—not OSI open source today—under the [Argus Source-Available License 1.0 (ASAL-1.0)](LICENSE).

- **Individuals & Small Teams (< 100 members):** Free to use, modify, and self-host for both commercial and non-commercial purposes.
- **Enterprises (100+ members):** Requires a commercial license. Contact us at [licensing@argustest.com](mailto:licensing@argustest.com) to get set up.
- Releases automatically convert to the **MIT License** after 3 years.
