// Package main: models_by_provider.go adds an additive, OpenAI-compatible
// GET /v1/models/by-provider endpoint. Unlike the canonical /v1/models
// (which dedupes by model ID, so a model served by several credentials
// collapses into ONE entry whose owned_by is whichever provider registered
// last), this endpoint lists models PER auth credential without deduping
// and tags each entry with a human-facing provider label in owned_by.
//
// The motivating case: two Google sources (a Vertex service-account JSON
// file and a vertex-api-key) both register as provider="vertex",
// owned_by="google", and the JSON source's models are a subset of the
// api-key's — so on the canonical list they are indistinguishable and the
// overlap hides one source entirely. Here every (credential, model) pair
// is emitted, so an overlapping model like gemini-3.1-pro-preview appears
// under BOTH "Vertex (JSON)" and "Vertex (API)".
//
// Purely additive: lives only in cmd/cliproxyapi-embed, reads the public
// SDK auth manager plus the internal model registry (same module), and
// touches no upstream files — future upstream merges stay conflict-free.
package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// providerLabel maps an auth credential to the provider bucket shown in
// owned_by. The only non-obvious split is "vertex": a service-account JSON
// file vs a vertex-api-key entry both report Provider=="vertex", so we
// distinguish them by the markers the config synthesizer sets on the
// api-key auth (Label "vertex-apikey", an api_key attribute, or a
// "config:vertex-apikey" source). Everything else falls back to the
// model's own owned_by (anthropic / openai / nvidia / google / ...).
func providerLabel(a *coreauth.Auth, fallback string) string {
	if a == nil {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(a.Provider)) {
	case "vertex":
		if isVertexAPIKeyAuth(a) {
			return "Vertex (API)"
		}
		return "Vertex (JSON)"
	case "gemini-cli":
		return "Gemini"
	case "gemini":
		return "Gemini"
	case "aistudio":
		return "Gemini (AI Studio)"
	}
	if fallback != "" {
		return fallback
	}
	return strings.TrimSpace(a.Provider)
}

// isVertexAPIKeyAuth reports whether a provider=="vertex" auth is the
// config vertex-api-key entry (as opposed to a service-account JSON file).
func isVertexAPIKeyAuth(a *coreauth.Auth) bool {
	if a == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(a.Label), "vertex-apikey") {
		return true
	}
	if a.Attributes != nil {
		if strings.TrimSpace(a.Attributes["api_key"]) != "" {
			return true
		}
		if strings.HasPrefix(a.Attributes["source"], "config:vertex-apikey") {
			return true
		}
	}
	return false
}

// modelsByProviderHandler serves GET /v1/models/by-provider. Output is the
// standard OpenAI model-list shape (object:"list", data:[{id,object,
// created,owned_by}]) so existing clients can group by owned_by, but with
// per-credential entries instead of a deduped union.
func modelsByProviderHandler(mgr *coreauth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if mgr == nil {
			c.JSON(http.StatusOK, gin.H{"object": "list", "data": []any{}})
			return
		}

		reg := registry.GetGlobalRegistry()
		data := make([]gin.H, 0, 64)
		for _, a := range mgr.List() {
			if a == nil || a.ID == "" {
				continue
			}
			models := reg.GetModelsForClient(a.ID)
			if len(models) == 0 {
				continue
			}
			for _, m := range models {
				if m == nil {
					continue
				}
				data = append(data, gin.H{
					"id":       m.ID,
					"object":   "model",
					"created":  m.Created,
					"owned_by": providerLabel(a, m.OwnedBy),
				})
			}
		}

		c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
	}
}
