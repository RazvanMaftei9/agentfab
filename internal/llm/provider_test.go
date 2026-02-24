package llm

import (
	"strings"
	"testing"

	"github.com/razvanmaftei/agentfab/internal/config"
)

func TestParseModelID(t *testing.T) {
	tests := []struct {
		input    string
		provider string
		model    string
		wantErr  bool
	}{
		{"anthropic/claude-opus-4", "anthropic", "claude-opus-4", false},
		{"openai/gpt-4o", "openai", "gpt-4o", false},
		{"openai-compat/llama-3", "openai-compat", "llama-3", false},
		{"anthropic/claude-3.5-sonnet-20241022", "anthropic", "claude-3.5-sonnet-20241022", false},
		{"no-slash", "", "", true},
		{"/no-provider", "", "", true},
		{"no-model/", "", "", true},
		{"", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			p, m, err := ParseModelID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p != tc.provider {
				t.Errorf("provider: got %q, want %q", p, tc.provider)
			}
			if m != tc.model {
				t.Errorf("model: got %q, want %q", m, tc.model)
			}
		})
	}
}

func TestDefaultMaxTokens(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		want     int
	}{
		{"anthropic", "claude-opus-4-6", 128_000},
		{"anthropic", "claude-opus-4-6-20260201", 128_000},
		{"anthropic", "claude-sonnet-4-6", 64_000},
		{"anthropic", "claude-sonnet-4-5-20250620", 64_000},
		{"anthropic", "claude-opus-4-5", 64_000},
		{"anthropic", "claude-haiku-4-5-20251001", 64_000},
		{"anthropic", "claude-opus-4-1", 32_000},
		{"anthropic", "claude-sonnet-4-0", 64_000},
		{"anthropic", "claude-sonnet-4", 64_000},
		{"anthropic", "claude-opus-4-0", 32_000},
		{"anthropic", "claude-opus-4", 32_000},
		{"openai", "gpt-4.1", 32_768},
		{"openai", "gpt-4.1-mini", 32_768},
		{"openai", "gpt-4.1-nano", 32_768},
		{"openai", "gpt-4o", 16_384},
		{"openai", "gpt-4o-mini", 16_384},
	}

	for _, tc := range tests {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			got := defaultMaxTokens(tc.provider, tc.model)
			if got != tc.want {
				t.Errorf("defaultMaxTokens(%q, %q) = %d, want %d", tc.provider, tc.model, got, tc.want)
			}
		})
	}
}

func TestDefaultMaxTokensFallback(t *testing.T) {
	tests := []struct {
		provider string
		model    string
	}{
		{"anthropic", "claude-3-opus-20240229"},
		{"openai", "o1-preview"},
		{"openai-compat", "llama-3"},
		{"custom", "some-model"},
	}

	for _, tc := range tests {
		t.Run(tc.provider+"/"+tc.model, func(t *testing.T) {
			got := defaultMaxTokens(tc.provider, tc.model)
			if got != 16_384 {
				t.Errorf("defaultMaxTokens(%q, %q) = %d, want 16384", tc.provider, tc.model, got)
			}
		})
	}
}

func TestContextLimit(t *testing.T) {
	tests := []struct {
		modelID string
		want    int
	}{
		{"anthropic/claude-opus-4-6", 200_000},
		{"anthropic/claude-sonnet-4-5", 200_000},
		{"openai/gpt-4.1", 1_048_576},
		{"openai/gpt-4o", 128_000},
		{"openai/gpt-4o-mini", 128_000},
		{"google/gemini-2.0-flash", 1_048_576},
		{"google/gemini-1.5-pro", 2_097_152},
		{"google/gemini-3-flash", 1_048_576},
		{"openai-compat/llama-3", 128_000},
		{"invalid", 128_000}, // bad format falls back
	}

	for _, tc := range tests {
		t.Run(tc.modelID, func(t *testing.T) {
			got := ContextLimit(tc.modelID)
			if got != tc.want {
				t.Errorf("ContextLimit(%q) = %d, want %d", tc.modelID, got, tc.want)
			}
		})
	}
}

func TestMaxOutputTokens(t *testing.T) {
	tests := []struct {
		modelID string
		want    int
	}{
		{"anthropic/claude-opus-4-6", 128_000},
		{"anthropic/claude-sonnet-4-5", 64_000},
		{"openai/gpt-4.1", 32_768},
		{"openai/gpt-4o", 16_384},
		{"invalid", 16_384}, // bad format falls back
	}

	for _, tc := range tests {
		t.Run(tc.modelID, func(t *testing.T) {
			got := MaxOutputTokens(tc.modelID)
			if got != tc.want {
				t.Errorf("MaxOutputTokens(%q) = %d, want %d", tc.modelID, got, tc.want)
			}
		})
	}
}

func TestNewChatModelUnknownProvider(t *testing.T) {
	_, err := NewChatModel(nil, "unknown/model", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestNewChatModelMissingAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, err := NewChatModel(nil, "anthropic/claude-opus-4", nil, nil)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}

	t.Setenv("OPENAI_API_KEY", "")
	_, err = NewChatModel(nil, "openai/gpt-4o", nil, nil)
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestResolveProvider_Builtin(t *testing.T) {
	for _, name := range []string{"anthropic", "openai", "google", "openai-compat"} {
		got, err := ResolveProvider(name, nil)
		if err != nil {
			t.Errorf("ResolveProvider(%q): unexpected error: %v", name, err)
		}
		if got != name {
			t.Errorf("ResolveProvider(%q) = %q, want %q", name, got, name)
		}
	}
}

func TestResolveProvider_Alias(t *testing.T) {
	providers := map[string]config.ProviderDef{
		"ollama": {Type: "openai-compat"},
	}
	got, err := ResolveProvider("ollama", providers)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "openai-compat" {
		t.Errorf("got %q, want %q", got, "openai-compat")
	}
}

func TestResolveProvider_Unknown(t *testing.T) {
	_, err := ResolveProvider("unknown", nil)
	if err == nil {
		t.Fatal("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveProvider_MissingType(t *testing.T) {
	providers := map[string]config.ProviderDef{
		"ollama": {}, // no Type
	}
	_, err := ResolveProvider("ollama", providers)
	if err == nil {
		t.Fatal("expected error for missing type")
	}
	if !strings.Contains(err.Error(), "must have a type") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCustomAPIKeyEnv(t *testing.T) {
	t.Setenv("MY_CUSTOM_KEY", "sk-test-123")
	t.Setenv("ANTHROPIC_API_KEY", "")

	// Use custom env var — the model creation will fail because the key is
	// fake, but it should NOT fail with "ANTHROPIC_API_KEY is required".
	_, err := NewChatModel(nil, "anthropic/claude-opus-4", &ProviderConfig{APIKeyEnv: "MY_CUSTOM_KEY"}, nil)
	if err != nil && strings.Contains(err.Error(), "is required") {
		t.Fatalf("should have found the custom env var, got: %v", err)
	}
	// The error (if any) should be about the model creation, not missing key.
}

func TestCustomAPIKeyEnvViaProviders(t *testing.T) {
	t.Setenv("MY_CLAUDE_KEY", "sk-test-456")
	t.Setenv("ANTHROPIC_API_KEY", "")

	providers := map[string]config.ProviderDef{
		"anthropic": {APIKeyEnv: "MY_CLAUDE_KEY"},
	}
	_, err := NewChatModel(nil, "anthropic/claude-opus-4", nil, providers)
	if err != nil && strings.Contains(err.Error(), "is required") {
		t.Fatalf("should have picked up custom env var from providers, got: %v", err)
	}
}

func TestAliasWithCustomEnvVars(t *testing.T) {
	t.Setenv("OLLAMA_URL", "http://localhost:11434/v1")
	t.Setenv("OPENAI_COMPAT_BASE_URL", "")

	providers := map[string]config.ProviderDef{
		"ollama": {Type: "openai-compat", BaseURLEnv: "OLLAMA_URL"},
	}
	// This will try to connect to Ollama. We just verify it resolves the
	// provider correctly and picks up the base URL env var.
	_, err := NewChatModel(nil, "ollama/llama3", nil, providers)
	// Should not fail with "unknown provider" or "base URL is required"
	if err != nil {
		if strings.Contains(err.Error(), "unknown provider") {
			t.Fatalf("should have resolved ollama alias, got: %v", err)
		}
		if strings.Contains(err.Error(), "base URL is required") {
			t.Fatalf("should have picked up OLLAMA_URL, got: %v", err)
		}
	}
}
