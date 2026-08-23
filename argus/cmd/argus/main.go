package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/browser"
	"github.com/ace-foundry/argus-testing/argus/internal/runner"
	"github.com/ace-foundry/argus-testing/argus/internal/server"
	"github.com/ace-foundry/argus-testing/argus/internal/store"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "install-browser" {
		if err := browser.Install(); err != nil {
			log.Fatal(err)
		}
		return
	}
	databasePath, screenshotDir := pathsFromEnv()
	if err := os.MkdirAll(screenshotDir, 0o755); err != nil {
		log.Fatal(err)
	}
	runStore, err := store.Open(databasePath)
	if err != nil {
		log.Fatal(err)
	}
	defer runStore.Close()
	runRunner := runner.New(runStore, browser.NewPlaywrightFactory(), runnerOptionsFromEnv(screenshotDir))
	handler, err := server.New(runStore, runRunner, optionsFromEnv(screenshotDir))
	if err != nil {
		log.Fatal(err)
	}
	runRunner.SetPublisher(handler.Publish)

	httpServer := &http.Server{Addr: serverAddressFromEnv(), Handler: handler}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		handler.Close()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		handler.Close()
		handler.Wait()
		log.Fatal(err)
	}
	handler.Close()
	handler.Wait()
}

func pathsFromEnv() (databasePath, screenshotDir string) {
	dataDir := os.Getenv("ARGUS_DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	databasePath = os.Getenv("ARGUS_DB_PATH")
	if databasePath == "" {
		databasePath = filepath.Join(dataDir, "argus.db")
	}
	return databasePath, filepath.Join(filepath.Dir(databasePath), "screenshots")
}

func serverAddressFromEnv() string {
	if address := os.Getenv("ARGUS_ADDR"); address != "" {
		return address
	}
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8000"
}

func runnerOptionsFromEnv(screenshotDir string) runner.Options {
	return runner.Options{
		ScreenshotDir: screenshotDir,
		Model:         os.Getenv("GEMINI_MODEL"),
		GPTModel:      os.Getenv("OPENAI_MODEL"),
		KimiModel:     os.Getenv("KIMI_MODEL"),
		KimiBaseURL:   os.Getenv("KIMI_BASE_URL"),
	}
}

func optionsFromEnv(screenshotDir ...string) server.Options {
	staticDir := os.Getenv("ARGUS_STATIC_DIR")
	if staticDir == "" {
		staticDir = defaultStaticDir()
	}
	shots := filepath.Join("data", "screenshots")
	if len(screenshotDir) > 0 {
		shots = screenshotDir[0]
	}
	return server.Options{
		StaticDir:        staticDir,
		ScreenshotDir:    shots,
		Model:            os.Getenv("GEMINI_MODEL"),
		GeminiConfigured: os.Getenv("GEMINI_API_KEY") != "",
		OpenAIConfigured: os.Getenv("OPENAI_API_KEY") != "",
		KimiConfigured:   os.Getenv("KIMI_API_KEY") != "",
	}
}

func defaultStaticDir() string {
	directory, err := os.Getwd()
	if err != nil {
		return filepath.Join("argus", "static")
	}
	return findStaticDir(directory)
}

func findStaticDir(start string) string {
	directory := filepath.Clean(start)
	for {
		candidate := filepath.Join(directory, "argus", "static")
		if info, err := os.Stat(filepath.Join(candidate, "index.html")); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return filepath.Join("argus", "static")
		}
		directory = parent
	}
}
