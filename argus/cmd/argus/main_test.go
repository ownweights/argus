package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindStaticDirWalksFromProvidedDirectory(t *testing.T) {
	root := t.TempDir()
	want := filepath.Join(root, "argus", "static")
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(want, "index.html"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "argus", "cmd", "argus")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findStaticDir(nested); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOptionsReadProviderConfiguration(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-key")
	t.Setenv("GEMINI_MODEL", "gemini-test")
	t.Setenv("OPENAI_API_KEY", "openai-key")
	t.Setenv("OPENAI_MODEL", "gpt-test")
	t.Setenv("KIMI_API_KEY", "kimi-key")
	t.Setenv("KIMI_MODEL", "kimi-test")
	t.Setenv("KIMI_BASE_URL", "https://kimi.example/v1")
	serverOptions := optionsFromEnv()
	if serverOptions.Model != "gemini-test" || !serverOptions.GeminiConfigured || !serverOptions.OpenAIConfigured || !serverOptions.KimiConfigured {
		t.Fatalf("server options = %#v", serverOptions)
	}
	runnerOptions := runnerOptionsFromEnv("screenshots")
	if runnerOptions.Model != "gemini-test" || runnerOptions.GPTModel != "gpt-test" || runnerOptions.KimiModel != "kimi-test" || runnerOptions.KimiBaseURL != "https://kimi.example/v1" {
		t.Fatalf("runner options = %#v", runnerOptions)
	}
}

func TestPathsAndAddressFromEnv(t *testing.T) {
	t.Setenv("ARGUS_DATA_DIR", "state")
	t.Setenv("ARGUS_DB_PATH", "")
	database, screenshots := pathsFromEnv()
	if database != filepath.Join("state", "argus.db") || screenshots != filepath.Join("state", "screenshots") {
		t.Fatalf("default paths = %q, %q", database, screenshots)
	}
	t.Setenv("ARGUS_DB_PATH", filepath.Join("custom", "runs.db"))
	database, screenshots = pathsFromEnv()
	if database != filepath.Join("custom", "runs.db") || screenshots != filepath.Join("custom", "screenshots") {
		t.Fatalf("custom paths = %q, %q", database, screenshots)
	}
	t.Setenv("ARGUS_ADDR", "")
	t.Setenv("PORT", "8123")
	if address := serverAddressFromEnv(); address != ":8123" {
		t.Fatalf("PORT address = %q", address)
	}
	t.Setenv("ARGUS_ADDR", "127.0.0.1:9000")
	if address := serverAddressFromEnv(); address != "127.0.0.1:9000" {
		t.Fatalf("ARGUS_ADDR address = %q", address)
	}
}
