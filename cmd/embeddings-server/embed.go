// Package main: embed.go implements the /v1/embeddings HTTP handler and a
// small embedContent client. Two upstream backends are supported behind a
// single embedClient surface:
//
//   - Vertex AI via OAuth (service-account JSON, regional endpoint)
//   - Generative Language API via API key (global endpoint, ?key=...)
//
// The choice is driven entirely by embedConfig; the handler does not care
// which backend services a request.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	// vertexScope is the OAuth scope required to call Vertex AI.
	vertexScope = "https://www.googleapis.com/auth/cloud-platform"

	// defaultConcurrency bounds how many embedContent calls run in
	// parallel for a single request. Per-project QPS is small, so we
	// keep this conservative.
	defaultConcurrency = 6

	// perRequestTimeout caps the total time spent serving one
	// /v1/embeddings call, including all parallel sub-requests.
	perRequestTimeout = 60 * time.Second

	// perCallTimeout caps a single embedContent call.
	perCallTimeout = 25 * time.Second
)

// embedClient holds the authenticated HTTP client and the parameters
// needed to address the embedContent endpoint for whichever backend is
// configured.
type embedClient struct {
	mode       authMode
	httpClient *http.Client

	// Vertex OAuth mode
	projectID string
	region    string

	// Generative Language API key mode
	apiKey string

	// Shared
	modelID string
}

// newEmbedClient constructs the appropriate backend client from the
// loaded configuration. For Vertex OAuth it returns an oauth2-backed
// HTTP client that refreshes access tokens automatically; for the
// Generative Language API it uses the default HTTP client (auth is
// carried in the URL as ?key=).
func newEmbedClient(ctx context.Context, cfg *embedConfig) (*embedClient, error) {
	switch cfg.mode {
	case authVertexOAuth:
		saBytes, err := os.ReadFile(cfg.saJSONPath)
		if err != nil {
			return nil, fmt.Errorf("read service account JSON: %w", err)
		}
		creds, err := google.CredentialsFromJSON(ctx, saBytes, vertexScope)
		if err != nil {
			return nil, fmt.Errorf("parse service account JSON: %w", err)
		}
		return &embedClient{
			mode:       cfg.mode,
			httpClient: oauth2.NewClient(ctx, creds.TokenSource),
			projectID:  cfg.projectID,
			region:     cfg.region,
			modelID:    cfg.modelID,
		}, nil

	case authGenerativeAPIKey:
		return &embedClient{
			mode:       cfg.mode,
			httpClient: &http.Client{},
			apiKey:     cfg.apiKey,
			modelID:    cfg.modelID,
		}, nil

	default:
		return nil, fmt.Errorf("unsupported auth mode %q", cfg.mode)
	}
}

// endpoint composes the embedContent URL for the active backend. For
// Generative Language mode the API key is appended as a query parameter.
func (ec *embedClient) endpoint() string {
	switch ec.mode {
	case authVertexOAuth:
		return fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:embedContent",
			ec.region, ec.projectID, ec.region, ec.modelID,
		)
	case authGenerativeAPIKey:
		return fmt.Sprintf(
			"https://generativelanguage.googleapis.com/v1beta/models/%s:embedContent?key=%s",
			ec.modelID, url.QueryEscape(ec.apiKey),
		)
	default:
		return ""
	}
}

// embedRequestBody is the JSON payload both backends accept. Vertex
// ignores the `model` field (it is encoded in the URL); the Generative
// Language API recommends it. Sending it on Vertex is harmless.
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

// embedResponseBody mirrors the shape both backends return. The
// Generative Language and Vertex AI embedContent endpoints share this
// envelope: {"embedding": {"values": [...]}}.
type embedResponseBody struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

// embed performs one embedContent call for a single text input.
func (ec *embedClient) embed(ctx context.Context, text string, dim *int) ([]float64, error) {
	body := embedRequestBody{
		Content:              embedContent{Parts: []embedPart{{Text: text}}},
		OutputDimensionality: dim,
	}
	if ec.mode == authGenerativeAPIKey {
		// Generative Language API expects `models/<id>` here.
		body.Model = "models/" + ec.modelID
	}

	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, ec.endpoint(), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

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
	Embedding interface{} `json:"embedding"` // []float64 or base64 string
}

type openAIUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// parseInputs normalises the OpenAI `input` field to []string. The field
// may be a single string or a JSON array of strings.
func parseInputs(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing input field")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, fmt.Errorf("input string is empty")
		}
		return []string{s}, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		if len(arr) == 0 {
			return nil, fmt.Errorf("input array is empty")
		}
		for i, v := range arr {
			if v == "" {
				return nil, fmt.Errorf("input[%d] is empty", i)
			}
		}
		return arr, nil
	}
	return nil, fmt.Errorf("input must be a string or array of strings")
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

// embeddingsHandler returns a gin handler closure bound to the given
// embed client and config.
func embeddingsHandler(ec *embedClient, cfg *embedConfig) gin.HandlerFunc {
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

		var dim *int
		if req.Dimensions != nil {
			if *req.Dimensions <= 0 {
				writeError(c, http.StatusBadRequest, "invalid_request_error", "dimensions must be positive")
				return
			}
			v := *req.Dimensions
			dim = &v
		} else if cfg.hasDefaultDim {
			v := cfg.defaultDim
			dim = &v
		}

		switch req.EncodingFormat {
		case "", "float", "base64":
		default:
			writeError(c, http.StatusBadRequest, "invalid_request_error",
				`encoding_format must be "float" or "base64"`)
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), perRequestTimeout)
		defer cancel()

		// gemini-embedding-2 fuses parts into a single vector, so we MUST
		// dispatch one upstream call per input. Bounded concurrency keeps
		// latency reasonable for batches without tripping rate limits.
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
				vec, err := ec.embed(ctx, txt, dim)
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

		model := req.Model
		if model == "" {
			model = cfg.modelID
		}

		c.JSON(http.StatusOK, openAIEmbedResponse{
			Object: "list",
			Data:   items,
			Model:  model,
			Usage:  openAIUsage{}, // best-effort; embedContent does not return token counts
		})
	}
}
