// Package passthrough contains native API surface passthrough helpers.
package passthrough

import "strings"

const (
	SurfaceOpenAI         = "openai"
	SurfaceOpenAIResponse = "openai-response"
	SurfaceClaude         = "claude"
	SurfaceGemini         = "gemini"
	SurfaceGeminiCLI      = "gemini-cli"
)

const (
	ProviderOpenAI    = "openai"
	ProviderCodex     = "codex"
	ProviderClaude    = "claude"
	ProviderGemini    = "gemini"
	ProviderGeminiCLI = "gemini-cli"
)

var allowedProvidersBySurface = map[string]map[string]struct{}{
	SurfaceOpenAI: {
		ProviderOpenAI: {},
		ProviderCodex:  {},
	},
	SurfaceOpenAIResponse: {
		ProviderCodex: {},
	},
	SurfaceClaude: {
		ProviderClaude: {},
	},
	SurfaceGemini: {
		ProviderGemini: {},
	},
	SurfaceGeminiCLI: {
		ProviderGeminiCLI: {},
	},
}

// SupportedSurface reports whether a downstream API surface is in v1 passthrough scope.
func SupportedSurface(surface string) bool {
	_, ok := allowedProvidersBySurface[normalize(surface)]
	return ok
}

// AllowsProvider reports whether provider may serve surface in native passthrough mode.
func AllowsProvider(surface, provider string) bool {
	allowed, ok := allowedProvidersBySurface[normalize(surface)]
	if !ok {
		return false
	}
	_, ok = allowed[normalize(provider)]
	return ok
}

// FilterProviders keeps providers that may serve surface in native passthrough mode.
func FilterProviders(surface string, providers []string) []string {
	allowed, ok := allowedProvidersBySurface[normalize(surface)]
	if !ok || len(providers) == 0 {
		return nil
	}

	filtered := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		key := normalize(provider)
		if key == "" {
			continue
		}
		if _, ok := allowed[key]; !ok {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		filtered = append(filtered, key)
	}
	return filtered
}

func normalize(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
