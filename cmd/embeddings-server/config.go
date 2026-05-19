// Package main hosts the embeddings-server binary, an additive layer that
// adds an OpenAI-compatible POST /v1/embeddings endpoint backed by Google
// embedding models. Two upstream backends are supported:
//
//   - Vertex AI via OAuth (a service-account JSON file). Required for
//     gemini-embedding-2 and any future Vertex-only embedding models.
//   - Generative Language API via API key. Simpler to deploy; works with
//     text-embedding-004 / gemini-embedding-001 etc.
//
// No upstream files are modified; the binary registers the route through
// the public SDK's router-configurator option.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// authMode selects which Google embedding backend to call.
type authMode string

const (
	authVertexOAuth      authMode = "vertex_oauth"
	authGenerativeAPIKey authMode = "generative_api_key"
)

// embedConfig captures the embeddings-server settings sourced from
// environment variables. Exactly one of saJSONPath / apiKey is set.
type embedConfig struct {
	mode authMode

	// Vertex OAuth mode (mode == authVertexOAuth)
	saJSONPath string
	projectID  string
	region     string

	// Generative Language API key mode (mode == authGenerativeAPIKey)
	apiKey string

	// Shared
	modelID       string
	defaultDim    int // 0 means "do not send outputDimensionality unless the request asks"
	hasDefaultDim bool
	cliproxyCfg   string // path to the CLIProxyAPI config.yaml
}

// loadEmbedConfig reads the embeddings configuration from environment
// variables. Required fields produce an error; optional fields fall back
// to documented defaults. Auth mode is inferred from which credential
// envs are present.
func loadEmbedConfig() (*embedConfig, error) {
	cfg := &embedConfig{
		saJSONPath:  strings.TrimSpace(os.Getenv("GCP_SA_JSON_PATH")),
		projectID:   strings.TrimSpace(os.Getenv("GCP_PROJECT_ID")),
		region:      strings.TrimSpace(os.Getenv("GCP_REGION")),
		apiKey:      strings.TrimSpace(os.Getenv("GCP_API_KEY")),
		modelID:     strings.TrimSpace(os.Getenv("EMBED_MODEL_ID")),
		cliproxyCfg: strings.TrimSpace(os.Getenv("EMBED_CONFIG_PATH")),
	}

	hasSA := cfg.saJSONPath != ""
	hasKey := cfg.apiKey != ""
	switch {
	case hasSA && hasKey:
		return nil, errors.New("GCP_SA_JSON_PATH and GCP_API_KEY are mutually exclusive; set exactly one")
	case hasSA:
		cfg.mode = authVertexOAuth
		if cfg.projectID == "" {
			return nil, errors.New("GCP_PROJECT_ID is required when using GCP_SA_JSON_PATH")
		}
		if cfg.region == "" {
			cfg.region = "us-central1"
		}
		if cfg.modelID == "" {
			cfg.modelID = "gemini-embedding-2"
		}
	case hasKey:
		cfg.mode = authGenerativeAPIKey
		// The Generative Language API does not use a region or project.
		if cfg.modelID == "" {
			cfg.modelID = "text-embedding-004"
		}
		// Strip any "models/" prefix users may copy from the docs;
		// our URL template re-adds it.
		cfg.modelID = strings.TrimPrefix(cfg.modelID, "models/")
	default:
		return nil, errors.New("set GCP_SA_JSON_PATH (Vertex OAuth) or GCP_API_KEY (Generative Language API)")
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

// String returns a redacted, log-safe summary of the configuration. The
// API key (if any) and service-account JSON path are never logged in
// full — paths only show the basename, keys only show their length.
func (c *embedConfig) String() string {
	switch c.mode {
	case authVertexOAuth:
		return fmt.Sprintf(
			"mode=vertex_oauth sa_json=%s project=%s region=%s model=%s default_dim=%s",
			filepathBase(c.saJSONPath), c.projectID, c.region, c.modelID, dimStr(c),
		)
	case authGenerativeAPIKey:
		return fmt.Sprintf(
			"mode=generative_api_key api_key_len=%d model=%s default_dim=%s",
			len(c.apiKey), c.modelID, dimStr(c),
		)
	default:
		return "mode=<unset>"
	}
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dimStr(c *embedConfig) string {
	if !c.hasDefaultDim {
		return "<none>"
	}
	return strconv.Itoa(c.defaultDim)
}
