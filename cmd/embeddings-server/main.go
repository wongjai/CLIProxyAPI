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

func main() {
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

	embedCfg, err := loadEmbedConfig()
	if err != nil {
		return fmt.Errorf("load embed config: %w", err)
	}

	cfg, err := config.LoadConfig(embedCfg.cliproxyCfg)
	if err != nil {
		return fmt.Errorf("load cliproxy config %q: %w", embedCfg.cliproxyCfg, err)
	}

	// SIGINT/SIGTERM cancel the root context, which signals graceful
	// shutdown to cliproxy.Service.Run.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	vc, err := newEmbedClient(ctx, embedCfg)
	if err != nil {
		return fmt.Errorf("init embed client: %w", err)
	}
	fmt.Fprintln(os.Stderr, "embeddings-server: config:", embedCfg.String())
	embedHandler := embeddingsHandler(vc, embedCfg)

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(embedCfg.cliproxyCfg).
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
