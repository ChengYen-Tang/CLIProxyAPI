package passthrough

import (
	"reflect"
	"testing"
)

func TestSupportedSurface(t *testing.T) {
	tests := []struct {
		name    string
		surface string
		want    bool
	}{
		{name: "openai", surface: "openai", want: true},
		{name: "openai response", surface: "openai-response", want: true},
		{name: "claude", surface: "claude", want: true},
		{name: "gemini", surface: "gemini", want: true},
		{name: "gemini cli", surface: "gemini-cli", want: true},
		{name: "unknown", surface: "vertex", want: false},
		{name: "trim and case normalize", surface: " Gemini ", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SupportedSurface(tt.surface); got != tt.want {
				t.Fatalf("SupportedSurface(%q) = %v, want %v", tt.surface, got, tt.want)
			}
		})
	}
}

func TestAllowsProvider(t *testing.T) {
	tests := []struct {
		name     string
		surface  string
		provider string
		want     bool
	}{
		{name: "openai allows openai", surface: "openai", provider: "openai", want: true},
		{name: "openai allows codex", surface: "openai", provider: "codex", want: true},
		{name: "openai response allows codex", surface: "openai-response", provider: "codex", want: true},
		{name: "openai response excludes openai", surface: "openai-response", provider: "openai", want: false},
		{name: "claude allows claude", surface: "claude", provider: "claude", want: true},
		{name: "gemini excludes vertex", surface: "gemini", provider: "vertex", want: false},
		{name: "gemini excludes gemini cli", surface: "gemini", provider: "gemini-cli", want: false},
		{name: "gemini cli excludes gemini", surface: "gemini-cli", provider: "gemini", want: false},
		{name: "unsupported surface excludes provider", surface: "antigravity", provider: "antigravity", want: false},
		{name: "normalizes input", surface: " Gemini ", provider: " GEMINI ", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AllowsProvider(tt.surface, tt.provider); got != tt.want {
				t.Fatalf("AllowsProvider(%q, %q) = %v, want %v", tt.surface, tt.provider, got, tt.want)
			}
		})
	}
}

func TestFilterProviders(t *testing.T) {
	tests := []struct {
		name      string
		surface   string
		providers []string
		want      []string
	}{
		{
			name:      "openai keeps openai and codex",
			surface:   "openai",
			providers: []string{"gemini", "codex", "openai", "claude"},
			want:      []string{"codex", "openai"},
		},
		{
			name:      "openai response keeps only codex",
			surface:   "openai-response",
			providers: []string{"openai", "codex", "openai-compatibility"},
			want:      []string{"codex"},
		},
		{
			name:      "claude keeps only claude",
			surface:   "claude",
			providers: []string{"gemini", "claude", "openai"},
			want:      []string{"claude"},
		},
		{
			name:      "gemini excludes related non-native surfaces",
			surface:   "gemini",
			providers: []string{"vertex", "gemini-cli", "antigravity", "gemini"},
			want:      []string{"gemini"},
		},
		{
			name:      "gemini cli keeps only gemini cli",
			surface:   "gemini-cli",
			providers: []string{"gemini", "gemini-cli", "vertex"},
			want:      []string{"gemini-cli"},
		},
		{
			name:      "deduplicates and normalizes",
			surface:   " GEMINI ",
			providers: []string{" Gemini ", "gemini", " GEMINI "},
			want:      []string{"gemini"},
		},
		{
			name:      "unsupported surface returns nil",
			surface:   "vertex",
			providers: []string{"vertex"},
			want:      nil,
		},
		{
			name:      "no matching provider returns empty slice",
			surface:   "gemini",
			providers: []string{"vertex"},
			want:      []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterProviders(tt.surface, tt.providers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("FilterProviders(%q, %v) = %v, want %v", tt.surface, tt.providers, got, tt.want)
			}
		})
	}
}
