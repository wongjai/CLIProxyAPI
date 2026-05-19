// Package main: embed.go implements the /v1/embeddings HTTP handler and a
// small Vertex AI embedContent client. The handler is intentionally
// self-contained: it does NOT integrate with CLIProxyAPI's auth manager,
// per the constraint that this is a purely additive layer.
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
	// parallel for a single request. Vertex per-project QPS is small,
	// so we keep this conservative.
	defaultConcurrency = 6

	// perRequestTimeout caps the total time spent serving one
	// /v1/embeddings call, including all parallel sub-requests.
	perRequestTimeout = 60 * time.Second

	// perCallTimeout caps a single Vertex embedContent call.
	perCallTimeout = 25 * time.Second
)

// vertexClient holds the authenticated HTTP client and the parameters
// needed to address the embedContent endpoint.
type vertexClient struct {
	httpClient *http.Client
	projectID  string
	region     string
	modelID    string
}

// newVertexClient builds an oauth2-backed HTTP client from a service-
// account JSON file. The returned client refreshes access tokens
// automatically via the oauth2 TokenSource.
func newVertexClient(ctx context.Context, cfg *embedConfig) (*vertexClient, error) {
	saBytes, err := os.ReadFile(cfg.saJSONPath)
	if err != nil {
		return nil, fmt.Errorf("read service account JSON: %w", err)
	}
	creds, err := google.CredentialsFromJSON(ctx, saBytes, vertexScope)
	if err != nil {
		return nil, fmt.Errorf("parse service account JSON: %w", err)
	}
	return &vertexClient{
		httpClient: oauth2.NewClient(ctx, creds.TokenSource),
		projectID:  cfg.projectID,
		region:     cfg.region,
		modelID:    cfg.modelID,
	}, nil
}

// endpoint composes the regional embedContent URL.
func (vc *vertexClient) endpoint() string {
	return fmt.Sprintf(
		"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/google/models/%s:embedContent",
		vc.region, vc.projectID, vc.region, vc.modelID,
	)
}

// vertexEmbedRequest is the JSON payload Vertex expects per input.
// gemini-embedding-2 fuses all parts of a single content into one vector,
// so callers MUST send exactly one input per request.
type vertexEmbedRequest struct {
	Content              vertexContent `json:"content"`
	OutputDimensionality *int          `json:"outputDimensionality,omitempty"`
}

type vertexContent struct {
	Parts []vertexPart `json:"parts"`
}

type vertexPart struct {
	Text string `json:"text"`
}

// vertexEmbedResponse mirrors the shape documented for embedContent.
// MUST VERIFY against a real call if the upstream API changes.
type vertexEmbedResponse struct {
	Embedding struct {
		Values []float64 `json:"values"`
	} `json:"embedding"`
}

// embed performs one embedContent call for a single text input.
func (vc *vertexClient) embed(ctx context.Context, text string, dim *int) ([]float64, error) {
	body := vertexEmbedRequest{
		Content:              vertexContent{Parts: []vertexPart{{Text: text}}},
		OutputDimensionality: dim,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, perCallTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, vc.endpoint(), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := vc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &vertexHTTPError{status: resp.StatusCode, body: string(raw)}
	}

	var parsed vertexEmbedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode vertex response: %w (body=%s)", err, string(raw))
	}
	if len(parsed.Embedding.Values) == 0 {
		return nil, fmt.Errorf("vertex returned empty embedding (body=%s)", string(raw))
	}
	return parsed.Embedding.Values, nil
}

// vertexHTTPError carries the raw upstream status & body so the handler
// can translate it into an OpenAI-style error response.
type vertexHTTPError struct {
	status int
	body   string
}

func (e *vertexHTTPError) Error() string {
	return fmt.Sprintf("vertex http %d: %s", e.status, e.body)
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
	// Try string first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, fmt.Errorf("input string is empty")
		}
		return []string{s}, nil
	}
	// Then array of strings.
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

// writeError returns an OpenAI-style error envelope. The HTTP status code
// mirrors OpenAI conventions: 400 for client mistakes, 502 for upstream
// failures, 429 passed through for rate-limit signalling, etc.
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
// Vertex client and embeddings config. The closure does no per-request
// auth: incoming auth is whatever CLIProxyAPI itself enforces on routes
// it registers after the default router setup.
func embeddingsHandler(vc *vertexClient, cfg *embedConfig) gin.HandlerFunc {
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

		// Resolve outputDimensionality: request wins, env default fills in.
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
			// supported
		default:
			writeError(c, http.StatusBadRequest, "invalid_request_error",
				`encoding_format must be "float" or "base64"`)
			return
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), perRequestTimeout)
		defer cancel()

		// gemini-embedding-2 fuses parts into a single vector, so we MUST
		// dispatch one Vertex call per input. Bounded concurrency keeps
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
				vec, err := vc.embed(ctx, txt, dim)
				if err != nil {
					errs[idx] = err
					return
				}
				results[idx] = vec
			}(i, text)
		}
		wg.Wait()

		// Surface the first error encountered. Rate-limit signals are
		// preserved with their original 429 status code; everything else
		// reports as 502 (upstream failure).
		for _, e := range errs {
			if e == nil {
				continue
			}
			status := http.StatusBadGateway
			errType := "upstream_error"
			if ve, ok := e.(*vertexHTTPError); ok {
				if ve.status == http.StatusTooManyRequests {
					status = http.StatusTooManyRequests
					errType = "rate_limit_exceeded"
				} else if ve.status >= 400 && ve.status < 500 {
					status = ve.status
					errType = "upstream_client_error"
				}
			}
			writeError(c, status, errType, e.Error())
			return
		}

		// Pack results in the requested encoding, preserving input order.
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
			Usage:  openAIUsage{}, // best-effort: Vertex embedContent does not return token counts here
		})
	}
}
