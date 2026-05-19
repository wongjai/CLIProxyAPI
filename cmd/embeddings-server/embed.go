// Package main: embed.go implements the /v1/embeddings HTTP handler and a
// small embedContent client. Credentials are picked up at request time
// (with mtime-based caching of config.yaml) so adding a key via the
// CLIProxyAPI management UI takes effect immediately.
//
// Backend selection (priority order):
//
//  1. auths/<file>.json with type=vertex
//     → real Vertex AI OAuth. The management UI's "Vertex AI service
//     account" upload writes this file. ServiceAccount JSON inside
//     drives token refresh; ProjectID & Location come from the
//     surrounding wrapper.
//  2. cfg.VertexCompatAPIKey[0]
//     → third-party Vertex-compatible provider via x-goog-api-key
//     header. Requires a non-empty base-url because real Vertex AI
//     API keys cannot call embedContent (Google requires OAuth).
//  3. cfg.GeminiKey[0]
//     → Generative Language API via ?key=<APIKey>.
//  4. env GCP_SA_JSON_PATH + GCP_PROJECT_ID + GCP_REGION
//     → real Vertex AI OAuth from a path on disk.
//  5. env GCP_API_KEY
//     → Generative Language fallback.
//
// If none is configured, /v1/embeddings responds 503 with a clear message
// rather than the binary refusing to boot.
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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	vertexScope        = "https://www.googleapis.com/auth/cloud-platform"
	defaultConcurrency = 6
	perRequestTimeout  = 60 * time.Second
	perCallTimeout     = 25 * time.Second

	// Default model IDs per backend when neither the request nor the
	// EMBED_MODEL_ID env specifies one.
	defaultModelGenLang = "text-embedding-004"
	defaultModelVertex  = "gemini-embedding-2"

	defaultGenLangBaseURL = "https://generativelanguage.googleapis.com"
)

// authMode selects which Google embedding backend to call.
type authMode string

const (
	authVertexOAuth        authMode = "vertex_oauth"
	authVertexCompatAPIKey authMode = "vertex_compat_api_key"
	authGenerativeAPIKey   authMode = "generative_api_key"
)

// credential is the immutable input to newEmbedClient.
type credential struct {
	mode authMode

	// Vertex OAuth
	saJSONBytes []byte // raw service-account JSON for google.CredentialsFromJSON
	projectID   string
	region      string

	// Vertex-compat API key (mode == authVertexCompatAPIKey)
	// Uses apiKey + baseURL; sends x-goog-api-key header.

	// Generative Language API key (mode == authGenerativeAPIKey)
	// Uses apiKey + (optional) baseURL.

	apiKey  string
	baseURL string
	headers map[string]string

	modelDefault string
	source       string
}

// fingerprint hashes everything that affects the upstream call so the
// resolver knows when to rebuild the cached client. Secrets are folded
// into the hash, never logged verbatim.
func (c *credential) fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|", c.mode)
	h.Write(c.saJSONBytes)
	fmt.Fprintf(h, "|%s|%s|%s|", c.projectID, c.region, c.baseURL)
	h.Write([]byte(c.apiKey))
	keys := make([]string, 0, len(c.headers))
	for k := range c.headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "|%s=%s", k, c.headers[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// resolver holds env fallbacks plus a small mtime-keyed cache of
// config.yaml + auth dir scans, and lazily builds/replaces an embedClient
// when the active credential changes.
type resolver struct {
	env *envSettings

	mu sync.Mutex
	// config.yaml cache
	cfgMtime time.Time
	cfg      *sdkconfig.Config

	// auth-dir scan cache (mtime keyed)
	authMtime  time.Time
	authVertex *vertexAuthFile

	// active client cache
	client      *embedClient
	clientPrint string
}

func newResolver(env *envSettings) *resolver {
	return &resolver{env: env}
}

// vertexAuthFile is the on-disk format the management UI writes for a
// Vertex service-account credential. We only care about the SA JSON and
// the project/location wrapper fields.
type vertexAuthFile struct {
	ServiceAccount map[string]any `json:"service_account"`
	ProjectID      string         `json:"project_id"`
	Location       string         `json:"location"`
	Type           string         `json:"type"`
	path           string         // populated post-decode
}

// refreshConfig stat()s the config file and reloads via the public SDK
// loader if mtime changed. Errors are non-fatal; we keep the old cache.
func (r *resolver) refreshConfig() {
	info, err := os.Stat(r.env.cliproxyCfg)
	if err != nil {
		r.cfg = nil
		return
	}
	if r.cfg != nil && info.ModTime().Equal(r.cfgMtime) {
		return
	}
	newCfg, err := sdkconfig.LoadConfig(r.env.cliproxyCfg)
	if err != nil {
		return
	}
	r.cfg = newCfg
	r.cfgMtime = info.ModTime()
}

// expandAuthDir resolves the auth-dir setting, expanding a leading "~"
// to the current user's home (mirroring internal/util.ResolveAuthDir).
func expandAuthDir(raw string) string {
	if raw == "" {
		raw = "~/.cli-proxy-api"
	}
	if strings.HasPrefix(raw, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Clean(raw)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(raw, "~"), "/\\")
		if rest == "" {
			return filepath.Clean(home)
		}
		return filepath.Clean(filepath.Join(home, filepath.FromSlash(rest)))
	}
	return filepath.Clean(raw)
}

// refreshAuthScan walks the auth directory and picks the first vertex
// credential file (sorted alphabetically for determinism). The scan is
// keyed by the directory's mtime so we only re-walk when files change.
func (r *resolver) refreshAuthScan() {
	authDir := ""
	if r.cfg != nil {
		authDir = r.cfg.AuthDir
	}
	authDir = expandAuthDir(authDir)

	info, err := os.Stat(authDir)
	if err != nil {
		r.authVertex = nil
		return
	}
	if r.authVertex != nil && info.ModTime().Equal(r.authMtime) {
		return
	}
	r.authMtime = info.ModTime()
	r.authVertex = nil

	entries, err := os.ReadDir(authDir)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		full := filepath.Join(authDir, name)
		data, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var file vertexAuthFile
		if err := json.Unmarshal(data, &file); err != nil {
			continue
		}
		if !strings.EqualFold(file.Type, "vertex") || len(file.ServiceAccount) == 0 {
			continue
		}
		file.path = full
		r.authVertex = &file
		return
	}
}

// selectCredential walks the priority chain and returns the first
// credential source that has the data it needs. Returns nil when nothing
// is configured.
func (r *resolver) selectCredential() *credential {
	// 1. Vertex SA JSON from auths/
	if r.authVertex != nil {
		saBytes, err := json.Marshal(r.authVertex.ServiceAccount)
		if err == nil && len(saBytes) > 0 {
			region := strings.TrimSpace(r.authVertex.Location)
			if region == "" {
				region = "us-central1"
			}
			return &credential{
				mode:         authVertexOAuth,
				saJSONBytes:  saBytes,
				projectID:    strings.TrimSpace(r.authVertex.ProjectID),
				region:       region,
				modelDefault: defaultModelVertex,
				source:       "auths:" + filepath.Base(r.authVertex.path),
			}
		}
	}

	if r.cfg != nil {
		// 2. Vertex-compat API key
		for i, k := range r.cfg.VertexCompatAPIKey {
			key := strings.TrimSpace(k.APIKey)
			if key == "" {
				continue
			}
			return &credential{
				mode:         authVertexCompatAPIKey,
				apiKey:       key,
				baseURL:      strings.TrimSpace(k.BaseURL),
				headers:      k.Headers,
				modelDefault: defaultModelVertex,
				source:       fmt.Sprintf("config.vertex-api-key[%d]", i),
			}
		}
		// 3. Generative Language API key
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

	// 4. Env Vertex OAuth
	if r.env.saJSONPath != "" && r.env.projectID != "" {
		data, err := os.ReadFile(r.env.saJSONPath)
		if err == nil && len(data) > 0 {
			region := r.env.region
			if region == "" {
				region = "us-central1"
			}
			return &credential{
				mode:         authVertexOAuth,
				saJSONBytes:  data,
				projectID:    r.env.projectID,
				region:       region,
				modelDefault: defaultModelVertex,
				source:       "env:GCP_SA_JSON_PATH",
			}
		}
	}
	// 5. Env Generative Language API key
	if r.env.apiKey != "" {
		return &credential{
			mode:         authGenerativeAPIKey,
			apiKey:       r.env.apiKey,
			modelDefault: defaultModelGenLang,
			source:       "env:GCP_API_KEY",
		}
	}
	return nil
}

// resolveClient returns the active embedClient, rebuilding it if the
// credential fingerprint changed since the last call.
func (r *resolver) resolveClient(ctx context.Context) (*embedClient, *credential, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.refreshConfig()
	r.refreshAuthScan()

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

// embedClient holds the authenticated HTTP client and the URL/header
// template for whichever backend is configured.
type embedClient struct {
	mode       authMode
	httpClient *http.Client

	// Vertex OAuth
	projectID string
	region    string

	// API-key modes
	apiKey  string
	baseURL string

	headers map[string]string
}

func newEmbedClient(ctx context.Context, cred *credential) (*embedClient, error) {
	switch cred.mode {
	case authVertexOAuth:
		if cred.projectID == "" {
			return nil, errors.New("vertex oauth: project_id is empty (set it in the auth file or GCP_PROJECT_ID)")
		}
		creds, err := google.CredentialsFromJSON(ctx, cred.saJSONBytes, vertexScope)
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

	case authVertexCompatAPIKey:
		if strings.TrimSpace(cred.baseURL) == "" {
			return nil, errors.New("vertex-api-key requires base-url: real Vertex AI does not accept API keys for embedContent; set base-url to a third-party Vertex-compatible provider")
		}
		return &embedClient{
			mode:       cred.mode,
			httpClient: &http.Client{},
			apiKey:     cred.apiKey,
			baseURL:    cred.baseURL,
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
	case authVertexCompatAPIKey:
		base := strings.TrimRight(ec.baseURL, "/")
		return fmt.Sprintf("%s/v1/publishers/google/models/%s:embedContent", base, modelID)
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

// embedRequestBody is the JSON payload all backends accept.
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

type embedResponseBody struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

func (ec *embedClient) embed(ctx context.Context, modelID, text string, dim *int) ([]float64, error) {
	body := embedRequestBody{
		Content:              embedContent{Parts: []embedPart{{Text: text}}},
		OutputDimensionality: dim,
	}
	if ec.mode == authGenerativeAPIKey {
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
	if ec.mode == authVertexCompatAPIKey {
		req.Header.Set("x-goog-api-key", ec.apiKey)
	}
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

type upstreamHTTPError struct {
	status int
	body   string
}

func (e *upstreamHTTPError) Error() string {
	return fmt.Sprintf("upstream http %d: %s", e.status, e.body)
}

type openAIEmbedRequest struct {
	Model          string          `json:"model"`
	Input          json.RawMessage `json:"input"`
	Dimensions     *int            `json:"dimensions,omitempty"`
	EncodingFormat string          `json:"encoding_format,omitempty"`
}

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

func encodeBase64Vector(values []float64) string {
	buf := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(float32(v)))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

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
				"no embedding credentials configured: upload a Vertex AI service account via the management UI, add a vertex-api-key or gemini-api-key entry, or set GCP_SA_JSON_PATH / GCP_API_KEY env")
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
