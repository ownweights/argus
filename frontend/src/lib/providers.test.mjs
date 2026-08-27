import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const require = createRequire(import.meta.url);
const sourcePath = fileURLToPath(new URL("./providers.ts", import.meta.url));
const tempDirectory = mkdtempSync(join(tmpdir(), "argus-providers-"));
const outputPath = join(tempDirectory, "providers.cjs");

try {
  const source = readFileSync(sourcePath, "utf8");
  const output = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
  }).outputText;
  writeFileSync(outputPath, output);
} catch (error) {
  rmSync(tempDirectory, { recursive: true, force: true });
  throw error;
}

const { providerCatalog, providerLabel, selectedProvider } = require(outputPath);

test.after(() => rmSync(tempDirectory, { recursive: true, force: true }));

test("uses Gemini as the safe default when settings are unavailable", () => {
  assert.deepEqual(providerCatalog(), [
    { id: "gemini", available: true, default: true },
    { id: "gpt", available: false, default: false },
    { id: "kimi", available: false, default: false },
    { id: "glm", available: false, default: false },
  ]);
});

test("keeps only configured providers selectable and sends the selected available provider", () => {
  const providers = providerCatalog([
    { id: "gemini", available: false, default: true },
    { id: "gpt", available: true, default: false },
    { id: "kimi", available: false, default: false },
  ]);

  assert.equal(selectedProvider("gpt", providers), "gpt");
  assert.equal(selectedProvider("gemini", providers), "gpt");
  assert.deepEqual({ provider: selectedProvider("kimi", providers) }, { provider: "gpt" });
});

test("falls back to the Gemini label for runs created before provider support", () => {
  assert.equal(providerLabel(), "Gemini");
  assert.equal(providerLabel("kimi"), "Kimi");
  assert.equal(providerLabel("glm"), "GLM-5.3 Flash");
});
