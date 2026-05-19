// Package main: embed.go implements the /v1/embeddings HTTP handler and a
// small embedContent client. Credentials are picked up at request time
// (with mtime-based caching of config.yaml) so adding a Gemini API key
// via the CLIProxyAPI management UI takes effect immediately.
//
// Two upstream paths are supported and chosen by priority:
//
//  1. cfg.GeminiKey   (config.yaml `gemini-api-key:` entry, also from
//     management UI). Calls the Generative Language API
//     with ?key=<APIKey>.
//  2. env GCP_API_KEY (same as above, but from environment).
//  3. env GCP_SA_JSON_PATH (+ GCP_PROJECT_ID, GCP_REGION). Calls real
//     Vertex AI with an OAuth bearer token.
//
// If none of the three is configured, the handler responds 503 with a
// clear message rather than the binary refusing to start.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// vertexScope is the OAuth scope required to call Vertex AI.
	vertexScope = "https://www.googleapis.com/auth/cloud-platform"

	// defaultConcurrency bounds how many embedContent calls run in
	// parallel for a single request.
	defaultConcurrency = 6

	// perRequestTimeout caps the total time spent serving one
	// /v1/embeddings call.
	perRequestTimeout = 60 * time.Second

	// perCallTimeout caps a single embedContent call.
	perCallTimeout = 25 * time.Second

	// Default model IDs per backend when neither the request nor the
	// EMBED_MODEL_ID env specifies one.
	defaultModelGenLang = "text-embedding-004"
	defaultModelVertex  = "gemini-embedding-2"

	// Default base URLs.
	defaultGenLangBaseURL = "https://generativelanguage.googleapis.com"
)

// authMode selects which Google embedding backend to call.
type authMode string

const (
	authVertexOAuth      authMode = "vertex_oauth"
	authGenerativeAPIKey authMode = "generative_api_key"
)

// credential is the immutable input to newEmbedClient. Fingerprint lets
// the resolver detect when the underlying source changed and rebuild the
// client only then.
type credential struct {
	mode         authMode
	apiKey       string
	saJSONPath   string
	projectID    string
	region       string
	baseURL      string // empty → use backend default
	headers      map[string]string
	modelDefault string
	source       string // human-readable origin for logs ("config.gemini-api-key[0]", "env:GCP_API_KEY", ...)
}

// fingerprint returns a hash that changes whenever any field that
// affects upstream calls changes. apiKey and saJSONPath contribute via
// hash to avoid logging raw secrets when this string surfaces.
func (c *credential) fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%s|%s|", c.mode, c.apiKey, c.saJSONPath, c.projectID, c.region, c.baseURL)
	for k, v := range c.headers {
		fmt.Fprintf(h, "%s=%s;", k, v)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolver holds the env fallbacks plus a tiny mtime-keyed cache of
// config.yaml, and lazily builds/replaces an embedClient when the active
// credential changes.
type resolver struct {
	env *envSettings

	mu sync.Mutex
	// config.yaml cache
	cfgMtime time.Time
	cfg      *sdkconfig.Config
	// active client cache
	client      *embedClient
	clientPrint string
}

func newResolver(env *envSettings) *resolver {
	return &resolver{env: env}
}

// refreshConfig stat()s the config file and reloads it if the mtime
// changed. The reload uses the public SDK loader so we never reach into
// internal/config. Errors during reload are non-fatal: we keep the old
// cached cfg.
func (r *resolver) refreshConfig() {
	info, err := os.Stat(r.env.cliproxyCfg)
	if err != nil {
		// File missing/unreadable: drop any stale cfg so the resolver
		// falls back to env-only.
		r.cfg = nil
		return
	}
	if r.cfg != nil && info.ModTime().Equal(r.cfgMtime) {
		return
	}
	newCfg, err := sdkconfig.LoadConfig(r.env.cliproxyCfg)
	if err != nil {
		// Keep old cache on parse error.
		return
	}
	r.cfg = newCfg
	r.cfgMtime = info.ModTime()
}

// selectCredential walks the priority order and returns the first
// credential source that has the data it needs. Returns nil when nothing
// is configured.
func (r *resolver) selectCredential() *credential {
	if r.cfg != nil {
		for i, k := range r.cfg.GeminiKey {
			key := strings.TrimSpace(k.APIKey)
			if key == "" {
				continue
			}
			return &credential{
				mode:         authGenerativeAPIKey,
				apiKey:       key,
				baseURL:      strings.TrimSpace(k.BaseURL),
				headers:      k.Headers,
				modelDefault: defaultModelGenLang,
				source:       fmt.Sprintf("config.gemini-api-key[%d]", i),
			}
		}
	}
	if r.env.apiKey != "" {
		return &credential{
			mode:         authGenerativeAPIKey,
			apiKey:       r.env.apiKey,
			modelDefault: defaultModelGenLang,
			source:       "env:GCP_API_KEY",
		}
	}
	if r.env.saJSONPath != "" && r.env.projectID != "" {
		return &credential{
			mode:         authVertexOAuth,
			saJSONPath:   r.env.saJSONPath,
			projectID:    r.env.projectID,
			region:       r.env.region,
			modelDefault: defaultModelVertex,
			source:       "env:GCP_SA_JSON_PATH",
		}
	}
	return nil
}

// resolveClient returns the active embedClient, rebuilding it if the
// credential fingerprint changed since the last call. Returns nil
// *embedClient when no credentials are configured anywhere; the handler
// translates that to 503.
func (r *resolver) resolveClient(ctx context.Context) (*embedClient, *credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.refreshConfig()
	cred := r.selectCredential()
	if cred == nil {
		r.client = nil
		r.clientPrint = ""
		return nil, nil, nil
	}
	fp := cred.fingerprint()
	if r.client != nil && fp == r.clientPrint {
		return r.client, cred, nil
	}
	c, err := newEmbedClient(ctx, cred)
	if err != nil {
		return nil, cred, err
	}
	r.client = c
	r.clientPrint = fp
	return c, cred, nil
}

// embedClient holds the authenticated HTTP client and the URL template
// for whichever backend is configured.
type embedClient struct {
	mode       authMode
	httpClient *http.Client

	// Vertex OAuth fields
	projectID string
	region    string

	// Generative Language fields
	apiKey  string
	baseURL string // may be empty → defaultGenLangBaseURL

	// Shared
	headers map[string]string
}

// newEmbedClient constructs the appropriate backend client from a
// resolved credential.
func newEmbedClient(ctx context.Context, cred *credential) (*embedClient, error) {
	switch cred.mode {
	case authVertexOAuth:
		saBytes, err := os.ReadFile(cred.saJSONPath)
		if err != nil {
			return nil, fmt.Errorf("read service account JSON: %w", err)
		}
		creds, err := google.CredentialsFromJSON(ctx, saBytes, vertexScope)
		if err != nil {
			return nil, fmt.Errorf("parse service account JSON: %w", err)
		}
		return &embedClient{
			mode:       cred.mode,
			httpClient: oauth2.NewClient(ctx, creds.TokenSource),
			projectID:  cred.projectID,
			region:     cred.region,
			headers:    cred.headers,
		}, nil
	case authGenerativeAPIKey:
		return &embedClient{
			mode:       cred.mode,
			httpClient: &http.Client{},
			apiKey:     cred.apiKey,
			baseURL:    cred.baseURL,
			headers:    cred.headers,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cred.mode)
	}
}

// endpoint composes the embedContent URL for the given model.
func (ec *embedClient) endpoint(modelID string) string {
	switch ec.mode {
	case authVertexOAuth:
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:embedContent",
			ec.region, ec.projectID, ec.region, modelID,
		)
	case authGenerativeAPIKey:
		base := strings.TrimRight(ec.baseURL, "/")
		if base == "" {
			base = defaultGenLangBaseURL
		}
		return fmt.Sprintf("%s/v1beta/models/%s:embedContent?key=%s",
			base, modelID, url.QueryEscape(ec.apiKey))
	default:
		return ""
	}
}

// embedRequestBody is the JSON payload both backends accept.
//
// gemini-embedding-2 fuses all parts of a single content into one vector,
// so callers MUST send exactly one input per request.
type embedRequestBody struct {
	Model                string       `json:"model,omitempty"`
	Content              embedContent `json:"content"`
	OutputDimensionality *int         `json:"outputDimensionality,omitempty"`
}

type embedContent struct {
	Parts []embedPart `json:"parts"`
}

type embedPart struct {
	Text string `json:"text"`
}

// embedResponseBody mirrors the shape both backends return.
type embedResponseBody struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

// embed performs one embedContent call for a single text input.
func (ec *embedClient) embed(ctx context.Context, modelID, text string, dim *int) ([]float64, error) {
	body := embedRequestBody{
		Content:              embedContent{Parts: []embedPart{{Text: text}}},
		OutputDimensionality: dim,
	}
	if ec.mode == authGenerativeAPIKey {
		// Generative Language API expects `models/<id>` here.
		body.Model = "models/" + modelID
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, ec.endpoint(modelID), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range ec.headers {
		req.Header.Set(k, v)
	}

	resp, err := ec.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &upstreamHTTPError{status: resp.StatusCode, body: string(raw)}
	}

	var parsed embedResponseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode embed response: %w (body=%s)", err, string(raw))
	}
	if len(parsed.Embedding.Values) == 0 {
		return nil, fmt.Errorf("upstream returned empty embedding (body=%s)", string(raw))
	}
	return parsed.Embedding.Values, nil
}

// upstreamHTTPError carries the raw upstream status & body so the handler
// can translate it into an OpenAI-style error response.
type upstreamHTTPError struct {
	status int
	body   string
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream http %d: %s", e.status, e.body)
}

// openAIEmbedRequest accepts the OpenAI embeddings request shape. `Input`
// is left as RawMessage because OpenAI permits either a string or an
// array of strings.
type openAIEmbedRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

// openAIEmbedResponse is the OpenAI embeddings response shape.
type openAIEmbedResponse struct {
	Object string                `json:"object"`
	Data   []openAIEmbeddingItem `json:"data"`
	Model  string                `json:"model"`
	Usage  openAIUsage           `json:"usage"`
}

type openAIEmbeddingItem struct {
	Object    string      `json:"object"`
	Index     int         `json:"index"`
	Embedding interface{} `json:"embedding"`
}

type openAIUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// parseInputs normalises the OpenAI `input` field to []string.
func parseInputs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("missing input field")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, errors.New("input string is empty")
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 {
			return nil, errors.New("input array is empty")
		}
		for i, v := range arr {
			if v == "" {
				return nil, fmt.Errorf("input[%d] is empty", i)
			}
		}
		return arr, nil
	}
	return nil, errors.New("input must be a string or array of strings")
}

// encodeBase64Vector packs a float64 slice into IEEE-754 little-endian
// float32 bytes and base64-encodes the result, matching OpenAI's
// `encoding_format: "base64"` convention.
func encodeBase64Vector(values []float64) string {
	buf := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(v)))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// writeError returns an OpenAI-style error envelope.
func writeError(c *gin.Context, status int, errType, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"code":    status,
		},
	})
}

// resolveModelID picks the upstream model name. Priority: request body,
// EMBED_MODEL_ID env, credential default. The "models/" prefix that the
// Generative Language docs use is stripped so the same string works for
// both backends.
func resolveModelID(reqModel string, env *envSettings, cred *credential) string {
	m := strings.TrimSpace(reqModel)
	if m == "" {
		m = env.modelOverride
	}
	if m == "" {
		m = cred.modelDefault
	}
	return strings.TrimPrefix(m, "models/")
}

// embeddingsHandler returns a gin handler closure bound to a resolver.
// The closure does no per-request auth: incoming auth is whatever
// CLIProxyAPI itself enforces on routes registered after default setup.
func embeddingsHandler(r *resolver) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req openAIEmbedRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request_error", "malformed JSON: "+err.Error())
			return
		}

		inputs, err := parseInputs(req.Input)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), perRequestTimeout)
		defer cancel()

		client, cred, err := r.resolveClient(ctx)
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "configuration_error", err.Error())
			return
		}
		if client == nil {
			writeError(c, http.StatusServiceUnavailable, "configuration_error",
				"no embedding credentials configured: add a gemini-api-key entry via the management UI / config.yaml, or set GCP_API_KEY / GCP_SA_JSON_PATH")
			return
		}

		modelID := resolveModelID(req.Model, r.env, cred)

		var dim *int
		if req.Dimensions != nil {
			if *req.Dimensions <= 0 {
				writeError(c, http.StatusBadRequest, "invalid_request_error", "dimensions must be positive")
				return
			}
			v := *req.Dimensions
			dim = &v
		} else if r.env.hasDefaultDim {
			v := r.env.defaultDim
			dim = &v
		}

		switch req.EncodingFormat {
		case "", "float", "base64":
		default:
			writeError(c, http.StatusBadRequest, "invalid_request_error",
				`encoding_format must be "float" or "base64"`)
			return
		}

		// gemini-embedding-2 fuses parts into a single vector, so we MUST
		// dispatch one upstream call per input. Bounded concurrency keeps
		// latency reasonable without tripping rate limits.
		results := make([][]float64, len(inputs))
		errs := make([]error, len(inputs))
		var wg sync.WaitGroup
		sem := make(chan struct{}, defaultConcurrency)

		for i, text := range inputs {
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int, txt string) {
				defer wg.Done()
				defer func() { <-sem }()
				vec, err := client.embed(ctx, modelID, txt, dim)
				if err != nil {
					errs[idx] = err
					return
				}
				results[idx] = vec
			}(i, text)
		}
		wg.Wait()

		for _, e := range errs {
			if e == nil {
				continue
			}
			status := http.StatusBadGateway
			errType := "upstream_error"
			if ue, ok := e.(*upstreamHTTPError); ok {
				if ue.status == http.StatusTooManyRequests {
					status = http.StatusTooManyRequests
					errType = "rate_limit_exceeded"
				} else if ue.status >= 400 && ue.status < 500 {
					status = ue.status
					errType = "upstream_client_error"
				}
			}
			writeError(c, status, errType, e.Error())
			return
		}

		items := make([]openAIEmbeddingItem, len(results))
		for i, vec := range results {
			items[i] = openAIEmbeddingItem{Object: "embedding", Index: i}
			if req.EncodingFormat == "base64" {
				items[i].Embedding = encodeBase64Vector(vec)
			} else {
				items[i].Embedding = vec
			}
		}

		respModel := strings.TrimSpace(req.Model)
		if respModel == "" {
			respModel = modelID
		}

		c.JSON(http.StatusOK, openAIEmbedResponse{
			Object: "list",
			Data:   items,
			Model:  respModel,
			Usage:  openAIUsage{},
		})
	}
}
