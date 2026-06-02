// Package main hosts the cliproxyapi-embed binary, an additive layer that
// adds an OpenAI-compatible POST /v1/embeddings endpoint backed by Google
// embedding models. Credentials are resolved at request time from the
// live CLIProxyAPI config.yaml (so additions made via the management UI
// take effect without a restart) or, optionally, from environment
// variables as a fallback.
//
// No upstream files are modified; the binary registers the route through
// the public SDK's router-configurator option.
package main

import (
	"os"
	"strconv"
	"strings"
)

// envSettings captures optional environment-variable overrides. None of
// these are required: the binary boots even when nothing is set, and the
// handler returns 503 with a clear message until credentials become
// available (either via env or via config.yaml).
type envSettings struct {
	// CLIProxyAPI config.yaml path (defaults to "config.yaml").
	cliproxyCfg string

	// Fallback Generative Language API key when config.yaml has none.
	apiKey string

	// Fallback Vertex AI OAuth path + parameters.
	saJSONPath string
	projectID  string
	region     string

	// Override the embedding model when the request does not specify one.
	modelOverride string

	// Server-wide default outputDimensionality when the request omits it.
	defaultDim    int
	hasDefaultDim bool
}

// loadEnvSettings reads the optional environment variables. Anything
// missing simply means "not configured at this layer"; the resolver
// will look elsewhere.
func loadEnvSettings() *envSettings {
	s := &envSettings{
		cliproxyCfg:   strings.TrimSpace(os.Getenv("EMBED_CONFIG_PATH")),
		apiKey:        strings.TrimSpace(os.Getenv("GCP_API_KEY")),
		saJSONPath:    strings.TrimSpace(os.Getenv("GCP_SA_JSON_PATH")),
		projectID:     strings.TrimSpace(os.Getenv("GCP_PROJECT_ID")),
		region:        strings.TrimSpace(os.Getenv("GCP_REGION")),
		modelOverride: strings.TrimSpace(os.Getenv("EMBED_MODEL_ID")),
	}
	if s.cliproxyCfg == "" {
		s.cliproxyCfg = "config.yaml"
	}
	if s.region == "" {
		s.region = "us-central1"
	}
	if raw := strings.TrimSpace(os.Getenv("EMBED_DEFAULT_DIM")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			s.defaultDim = n
			s.hasDefaultDim = true
		}
	}
	return s
}
