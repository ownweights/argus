import type { ProviderID, ProviderInfo } from "../types";

const defaults: ProviderInfo[] = [
  { id: "gemini", available: true, default: true },
  { id: "gpt", available: false, default: false },
  { id: "kimi", available: false, default: false },
  { id: "glm", available: false, default: false },
];

export function providerCatalog(providers?: ProviderInfo[]): ProviderInfo[] {
  if (!providers?.length) return defaults;
  return defaults.map((fallback) => providers.find((provider) => provider.id === fallback.id) ?? { ...fallback, available: false });
}

export function selectedProvider(provider: ProviderID, providers: ProviderInfo[]): ProviderID | undefined {
  if (providers.some((item) => item.id === provider && item.available)) return provider;
  return providers.find((item) => item.available)?.id;
}

export function providerLabel(provider: ProviderID = "gemini") {
  return { gemini: "Gemini", gpt: "GPT", kimi: "Kimi", glm: "GLM-5.3 Flash" }[provider];
}
