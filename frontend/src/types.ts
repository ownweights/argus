import type { RunStatusValue } from "./constants";

export interface RunEvent {
  id: number;
  run_id: string;
  type: string;
  created_at: string;
  data: Record<string, unknown>;
}

export interface Finding {
  severity: string;
  title: string;
  detail: string;
}

export interface RunReport {
  verdict: "passed" | "failed" | "inconclusive";
  summary: string;
  plan?: string;
  findings: Finding[];
  recommendations: string[];
}

export type ProviderID = "gemini" | "gpt" | "kimi";

export interface ProviderInfo {
  id: ProviderID;
  available: boolean;
  default: boolean;
}

export interface RunAuthorization {
  allow_mutations?: boolean;
  allow_destructive?: boolean;
  allowed_origins?: string[];
  secret_bindings?: Record<string, string>;
}

export interface RunPolicy {
  allow_mutations: boolean;
  allow_destructive: boolean;
  allowed_origins: string[];
}

export interface CreateRunRequest {
  url: string;
  instructions: string;
  provider?: ProviderID;
  authorization?: RunAuthorization;
}

export interface Run {
  id: string;
  url: string;
  instructions: string;
  provider?: ProviderID;
  status: RunStatusValue;
  created_at: string;
  updated_at: string;
  error: string | null;
  report: RunReport | null;
  policy?: RunPolicy;
  events?: RunEvent[];
}

export interface Settings {
  gemini_configured: boolean;
  model: string;
  providers: ProviderInfo[];
}
