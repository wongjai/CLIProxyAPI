// Package translator provides request and response translation functionality
// between different AI API formats. It acts as a wrapper around the SDK translator
// registry, providing convenient functions for translating requests and responses
// between OpenAI, Claude, Gemini, and other API formats.
package translator

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// registry holds the default translator registry instance.
var registry = sdktranslator.Default()

// Register registers a new translator for converting between two API formats.
//
// Parameters:
//   - from: The source API format identifier
//   - to: The target API format identifier
//   - request: The request translation function
//   - response: The response translation function
func Register(from, to string, request interfaces.TranslateRequestFunc, response interfaces.TranslateResponse) {
	registry.Register(sdktranslator.FromString(from), sdktranslator.FromString(to), request, response)
}
