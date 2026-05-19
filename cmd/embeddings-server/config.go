// Package main hosts the embeddings-server binary, an additive layer that
// adds an OpenAI-compatible POST /v1/embeddings endpoint backed by Google
// Vertex AI on top of the standard CLIProxyAPI service. No upstream files
// are modified; the binary registers the route through the public SDK's
// router-configurator option.
package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// embedConfig captures the embeddings-server settings sourced from
// environment variables. Secrets (the service-account key) live on disk;
// only the path is held here.
type embedConfig struct {
	saJSONPath    string
	projectID     string
	region        string
	modelID       string
	defaultDim    int // 0 means "do not send outputDimensionality unless the request asks"
	hasDefaultDim bool
	cliproxyCfg   string // path to the CLIProxyAPI config.yaml
}

// loadEmbedConfig reads the embeddings configuration from environment
// variables. Required fields produce an error; optional fields fall back
// to documented defaults.
func loadEmbedConfig() (*embedConfig, error) {
	cfg := &embedConfig{
		saJSONPath:  strings.TrimSpace(os.Getenv("GCP_SA_JSON_PATH")),
		projectID:   strings.TrimSpace(os.Getenv("GCP_PROJECT_ID")),
		region:      strings.TrimSpace(os.Getenv("GCP_REGION")),
		modelID:     strings.TrimSpace(os.Getenv("EMBED_MODEL_ID")),
		cliproxyCfg: strings.TrimSpace(os.Getenv("EMBED_CONFIG_PATH")),
	}

	if cfg.saJSONPath == "" {
		return nil, errors.New("GCP_SA_JSON_PATH is required")
	}
	if cfg.projectID == "" {
		return nil, errors.New("GCP_PROJECT_ID is required")
	}
	if cfg.region == "" {
		cfg.region = "us-central1"
	}
	if cfg.modelID == "" {
		cfg.modelID = "gemini-embedding-2"
	}
	if cfg.cliproxyCfg == "" {
		cfg.cliproxyCfg = "config.yaml"
	}

	if raw := strings.TrimSpace(os.Getenv("EMBED_DEFAULT_DIM")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return nil, errors.New("EMBED_DEFAULT_DIM must be a positive integer")
		}
		cfg.defaultDim = n
		cfg.hasDefaultDim = true
	}

	return cfg, nil
}
