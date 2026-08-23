import { useEffect, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { ArrowUp, Globe } from "lucide-react";
import { useNavigate } from "react-router-dom";
import { api } from "../../lib/api";
import { providerCatalog, providerLabel, selectedProvider } from "../../lib/providers";
import type { CreateRunRequest, ProviderID, ProviderInfo, Run, Settings } from "../../types";

const suggestions = [
  "Check that navigation links work across all pages",
  "Verify the responsive layout on mobile",
  "Test the signup form validation",
  "Inspect the main user flow for visual issues",
];

export function NewTestInput() {
  const navigate = useNavigate();
  const textarea = useRef<HTMLTextAreaElement>(null);
  const [instructions, setInstructions] = useState("");
  const [url, setUrl] = useState("");
  const [focused, setFocused] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const [allowMutations, setAllowMutations] = useState(false);
  const [allowDestructive, setAllowDestructive] = useState(false);
  const [allowedOrigins, setAllowedOrigins] = useState("");
  const [providers, setProviders] = useState<ProviderInfo[]>(() => providerCatalog());
  const [provider, setProvider] = useState<ProviderID>("gemini");
  const activeProvider = selectedProvider(provider, providers);

  useEffect(() => {
    void api<Settings>("/api/settings").then((settings) => {
      const catalog = providerCatalog(settings.providers);
      setProviders(catalog);
      setProvider((current) => selectedProvider(current, catalog) ?? current);
    }).catch(() => undefined);
  }, []);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (!instructions.trim() || !url.trim() || submitting) return;
    if (!activeProvider) {
      setError("Configure a provider API key before starting a test");
      return;
    }
    setSubmitting(true);
    setError("");
    try {
      const origins = allowedOrigins.split("\n").map((value) => value.trim()).filter(Boolean);
      const request: CreateRunRequest = {
        url: url.trim(),
        instructions: instructions.trim(),
        provider: activeProvider,
        authorization: allowMutations || origins.length > 0 ? {
          allow_mutations: allowMutations,
          allow_destructive: allowMutations && allowDestructive,
          allowed_origins: origins,
        } : undefined,
      };
      const run = await api<Run>("/api/runs", {
        method: "POST",
        body: JSON.stringify(request),
      });
      navigate(`/runs/${run.id}`);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "Could not start test");
      setSubmitting(false);
    }
  };

  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void submit();
    }
  };

  return <form className={`composer ${focused ? "focused" : ""}`} onSubmit={(event) => void submit(event)}>
    <textarea
      ref={textarea}
      rows={3}
      required
      value={instructions}
      onChange={(event) => setInstructions(event.target.value)}
      onKeyDown={onKeyDown}
      onFocus={() => setFocused(true)}
      onBlur={() => setFocused(false)}
      placeholder={'Describe a test to run… e.g. "Test the checkout flow on mobile"'}
      aria-label="Test description"
    />
    <div className="url-field">
      <Globe size={13} />
      <input type="url" required value={url} onChange={(event) => setUrl(event.target.value)} placeholder="https://example.com" aria-label="Target URL" />
      <label className="provider-select">Provider
        <select value={provider} onChange={(event) => setProvider(event.target.value as ProviderID)} aria-label="AI provider">
          {providers.map((item) => <option key={item.id} value={item.id} disabled={!item.available}>{providerLabel(item.id)}{item.available ? "" : " — API key required"}</option>)}
        </select>
      </label>
    </div>
    <details className="run-policy">
      <summary>Run policy <span>{allowMutations ? "Mutations authorized" : "Read-only"}</span></summary>
      <div className="run-policy-fields">
        <label className="policy-toggle">
          <input
            type="checkbox"
            checked={allowMutations}
            onChange={(event) => {
              setAllowMutations(event.target.checked);
              if (!event.target.checked) setAllowDestructive(false);
            }}
          />
          <span><strong>Allow state-changing actions</strong><small>Permits form submissions and non-read-only network requests.</small></span>
        </label>
        <label className="policy-toggle">
          <input
            type="checkbox"
            checked={allowDestructive}
            disabled={!allowMutations}
            onChange={(event) => setAllowDestructive(event.target.checked)}
          />
          <span><strong>Allow destructive actions</strong><small>Separately permits controls classified as delete, remove, purchase, or equivalent.</small></span>
        </label>
        <label className="policy-origins-label" htmlFor="allowed-origins">Additional allowed origins</label>
        <textarea
          id="allowed-origins"
          className="policy-origins"
          rows={2}
          value={allowedOrigins}
          onChange={(event) => setAllowedOrigins(event.target.value)}
          placeholder={"https://accounts.example.com\nhttps://cdn.example.com"}
        />
        <small>One HTTP(S) origin per line. The target origin is always included. Secret bindings are accepted only through the API.</small>
      </div>
    </details>
    <div className="composer-toolbar">
      <span><Globe size={13} />Target URL</span>
      <span className="key-hint">Enter to run · Shift + Enter for a new line</span>
      <button type="submit" className="send-button" disabled={!instructions.trim() || !url.trim() || !activeProvider || submitting} title="Start test">
        <ArrowUp size={15} />
      </button>
    </div>
    {error && <div className="inline-error">{error}</div>}
    {!instructions && <div className="suggestions">{suggestions.map((suggestion) => <button type="button" key={suggestion} onMouseDown={(event) => {
      event.preventDefault();
      setInstructions(suggestion);
      textarea.current?.focus();
    }}>{suggestion}</button>)}</div>}
  </form>;
}
