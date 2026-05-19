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
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "embeddings-server:", err)
		os.Exit(1)
	}
}

func run() error {
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

	vc, err := newVertexClient(ctx, embedCfg)
	if err != nil {
		return fmt.Errorf("init vertex client: %w", err)
	}
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
