// Package main: main.go bootstraps the embeddings-server. It builds the
// standard CLIProxyAPI service via the public SDK and registers a
// POST /v1/embeddings route on top of the default router using
// WithRouterConfigurator. No upstream files are touched.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"

	// Register all built-in protocol translators (Gemini, Claude, OpenAI,
	// Codex, gemini-cli). Without this, the embedded cliproxy service can
	// accept requests on /v1/chat/completions and friends but cannot
	// translate them — making this binary a true superset of cmd/server.
	_ "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator/builtin"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

func init() {
	buildinfo.Version = Version
	buildinfo.Commit = Commit
	buildinfo.BuildDate = BuildDate
}

func main() {
	fmt.Printf("embeddings-server Version: %s, Commit: %s, BuiltAt: %s\n", Version, Commit, BuildDate)
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embeddings-server:", err)
		os.Exit(1)
	}
}

func run() error {
	// Mirror cmd/server: load .env from the working directory if present.
	// Errors other than "file not found" are non-fatal but surfaced.
	if wd, err := os.Getwd(); err == nil {
		if errLoad := godotenv.Load(filepath.Join(wd, ".env")); errLoad != nil && !errors.Is(errLoad, os.ErrNotExist) {
			fmt.Fprintln(os.Stderr, "embeddings-server: warning: load .env:", errLoad)
		}
	}

	env := loadEnvSettings()

	cfg, err := config.LoadConfig(env.cliproxyCfg)
	if err != nil {
		return fmt.Errorf("load cliproxy config %q: %w", env.cliproxyCfg, err)
	}

	// Wire the global logger to either rotating files (when
	// logging-to-file: true) or stdout. Without this, /v0/management/logs
	// shows nothing because the management UI reads main.log on disk.
	// Mirrors cmd/server's behaviour.
	if err := logging.ConfigureLogOutput(cfg); err != nil {
		return fmt.Errorf("configure log output: %w", err)
	}

	// SIGINT/SIGTERM cancel the root context, which signals graceful
	// shutdown to cliproxy.Service.Run.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The resolver picks credentials at request time from the live
	// config.yaml (mtime-cached), with env fallbacks. The binary boots
	// even when no credentials are configured anywhere; the handler
	// returns 503 with a clear message until creds appear.
	res := newResolver(env)
	embedHandler := embeddingsHandler(res)

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(env.cliproxyCfg).
		WithServerOptions(
			api.WithRouterConfigurator(func(e *gin.Engine, _ *handlers.BaseAPIHandler, _ *config.Config) {
				e.POST("/v1/embeddings", embedHandler)
			}),
		).
		Build()
	if err != nil {
		return fmt.Errorf("build cliproxy service: %w", err)
	}

	if err := svc.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("service run: %w", err)
	}
	return nil
}
