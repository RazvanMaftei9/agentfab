package llm

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	claude "github.com/cloudwego/eino-ext/components/model/claude"
	gemini "github.com/cloudwego/eino-ext/components/model/gemini"
	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/internal/config"
	"google.golang.org/genai"
)

var httpClient = &http.Client{
	Timeout: 10 * time.Minute,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		IdleConnTimeout:       90 * time.Second,
	},
}

// ProviderOptions returns provider-specific options (e.g. Anthropic prompt caching).
func ProviderOptions(modelID string, providers map[string]config.ProviderDef) []model.Option {
	provider, _, err := ParseModelID(modelID)
	if err != nil {
		return nil
	}
	base, err := ResolveProvider(provider, providers)
	if err != nil {
		return nil
	}
	switch base {
	case "anthropic":
		return []model.Option{claude.WithEnableAutoCache(true)}
	default:
		return nil
	}
}

// HasPromptCaching returns true if the provider supports prefix-based prompt caching.
func HasPromptCaching(modelID string, providers map[string]config.ProviderDef) bool {
	provider, _, err := ParseModelID(modelID)
	if err != nil {
		return false
	}
	base, err := ResolveProvider(provider, providers)
	if err != nil {
		return false
	}
	return base == "anthropic"
}

func ParseModelID(modelID string) (provider, modelName string, err error) {
	parts := strings.SplitN(modelID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("model ID %q must use provider/model-id format", modelID)
	}
	return parts[0], parts[1], nil
}

type ProviderConfig struct {
	APIKey     string
	BaseURL    string
	MaxTokens  int
	APIKeyEnv  string // custom env var name for API key
	BaseURLEnv string // custom env var name for base URL
}

var builtinProviders = map[string]bool{
	"anthropic": true, "openai": true, "google": true, "openai-compat": true,
}

// ResolveProvider maps a provider name (possibly an alias) to its base type.
func ResolveProvider(name string, providers map[string]config.ProviderDef) (string, error) {
	if builtinProviders[name] {
		return name, nil
	}
	pdef, ok := providers[name]
	if !ok {
		return "", fmt.Errorf("unknown provider %q (define it in providers: section of agents.yaml)", name)
	}
	if pdef.Type == "" {
		return "", fmt.Errorf("provider %q must have a type field (one of: anthropic, openai, google, openai-compat)", name)
	}
	if !builtinProviders[pdef.Type] {
		return "", fmt.Errorf("provider %q has invalid type %q", name, pdef.Type)
	}
	return pdef.Type, nil
}

func NewChatModel(ctx context.Context, modelID string, cfg *ProviderConfig, providers map[string]config.ProviderDef) (model.ChatModel, error) {
	providerName, modelName, err := ParseModelID(modelID)
	if err != nil {
		return nil, err
	}

	baseProvider, err := ResolveProvider(providerName, providers)
	if err != nil {
		return nil, err
	}

	if cfg == nil {
		cfg = &ProviderConfig{}
	}
	if pdef, ok := providers[providerName]; ok {
		if cfg.APIKeyEnv == "" {
			cfg.APIKeyEnv = pdef.APIKeyEnv
		}
		if cfg.BaseURLEnv == "" {
			cfg.BaseURLEnv = pdef.BaseURLEnv
		}
	}
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = defaultMaxTokens(baseProvider, modelName)
	}

	switch baseProvider {
	case "anthropic":
		return newAnthropicModel(ctx, modelName, cfg)
	case "openai":
		return newOpenAIModel(ctx, modelName, cfg)
	case "openai-compat":
		return newOpenAICompatModel(ctx, modelName, cfg)
	case "google":
		return newGoogleModel(ctx, modelName, cfg)
	default:
		return nil, fmt.Errorf("unknown provider %q", baseProvider)
	}
}

func defaultMaxTokens(provider, model string) int {
	switch provider {
	case "anthropic":
		switch {
		case strings.HasPrefix(model, "claude-opus-4-6"):
			return 128_000
		case strings.HasPrefix(model, "claude-sonnet-4-6"):
			return 64_000
		case strings.HasPrefix(model, "claude-sonnet-4-5"):
			return 64_000
		case strings.HasPrefix(model, "claude-opus-4-5"):
			return 64_000
		case strings.HasPrefix(model, "claude-haiku-4-5"):
			return 64_000
		case strings.HasPrefix(model, "claude-opus-4-1"):
			return 32_000
		case strings.HasPrefix(model, "claude-sonnet-4-0"),
			strings.HasPrefix(model, "claude-sonnet-4"):
			return 64_000
		case strings.HasPrefix(model, "claude-opus-4-0"),
			strings.HasPrefix(model, "claude-opus-4"):
			return 32_000
		default:
			return 16_384
		}
	case "openai":
		switch {
		case strings.HasPrefix(model, "gpt-5.3"),
			strings.HasPrefix(model, "gpt-5.2"):
			return 128_000
		case strings.HasPrefix(model, "gpt-5"):
			return 64_000
		case strings.HasPrefix(model, "gpt-4.1"):
			return 32_768
		case strings.HasPrefix(model, "gpt-4o"):
			return 16_384
		default:
			return 16_384
		}
	case "google":
		switch {
		case strings.HasPrefix(model, "gemini-3.1-pro"):
			return 65_536
		case strings.HasPrefix(model, "gemini-3-flash"):
			return 16384
		case strings.HasPrefix(model, "gemini-1.5-pro"), strings.HasPrefix(model, "gemini-2.0-flash-thinking"):
			return 8192
		case strings.HasPrefix(model, "gemini-2.0-flash"):
			return 8192
		default:
			return 8192
		}
	default:
		return 16_384
	}
}

// ContextLimit returns the context window size in tokens for a "provider/model-id".
func ContextLimit(modelID string, providers ...map[string]config.ProviderDef) int {
	provider, modelName, err := ParseModelID(modelID)
	if err != nil {
		return 128_000
	}
	var pmap map[string]config.ProviderDef
	if len(providers) > 0 {
		pmap = providers[0]
	}
	base, err := ResolveProvider(provider, pmap)
	if err != nil {
		return 128_000
	}
	return contextLimit(base, modelName)
}

func contextLimit(provider, model string) int {
	switch provider {
	case "anthropic":
		return 200_000
	case "openai":
		switch {
		case strings.HasPrefix(model, "gpt-5"):
			return 400_000
		case strings.HasPrefix(model, "gpt-4.1"):
			return 1_048_576
		case strings.HasPrefix(model, "gpt-4o"):
			return 128_000
		default:
			return 128_000
		}
	case "google":
		switch {
		case strings.HasPrefix(model, "gemini-3.1-pro"):
			return 2_097_152
		case strings.HasPrefix(model, "gemini-1.5-pro"):
			return 2_097_152
		case strings.HasPrefix(model, "gemini-2.0-flash"),
			strings.HasPrefix(model, "gemini-3-flash"):
			return 1_048_576
		default:
			return 1_048_576
		}
	default:
		return 128_000
	}
}

// MaxOutputTokens returns the maximum output tokens for a "provider/model-id".
func MaxOutputTokens(modelID string, providers ...map[string]config.ProviderDef) int {
	provider, modelName, err := ParseModelID(modelID)
	if err != nil {
		return 16_384
	}
	var pmap map[string]config.ProviderDef
	if len(providers) > 0 {
		pmap = providers[0]
	}
	base, err := ResolveProvider(provider, pmap)
	if err != nil {
		return 16_384
	}
	return defaultMaxTokens(base, modelName)
}

func newAnthropicModel(ctx context.Context, modelName string, cfg *ProviderConfig) (model.ChatModel, error) {
	apiKey := cfg.APIKey
	envName := "ANTHROPIC_API_KEY"
	if cfg.APIKeyEnv != "" {
		envName = cfg.APIKeyEnv
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("%s is required for anthropic provider", envName)
	}

	baseURL := cfg.BaseURL
	if baseURL == "" && cfg.BaseURLEnv != "" {
		baseURL = os.Getenv(cfg.BaseURLEnv)
	}

	config := &claude.Config{
		APIKey:     apiKey,
		Model:      modelName,
		MaxTokens:  cfg.MaxTokens,
		HTTPClient: httpClient,
	}
	if baseURL != "" {
		config.BaseURL = &baseURL
	}

	return claude.NewChatModel(ctx, config)
}

func newOpenAIModel(ctx context.Context, modelName string, cfg *ProviderConfig) (model.ChatModel, error) {
	apiKey := cfg.APIKey
	envName := "OPENAI_API_KEY"
	if cfg.APIKeyEnv != "" {
		envName = cfg.APIKeyEnv
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("%s is required for openai provider", envName)
	}

	baseURL := cfg.BaseURL
	if baseURL == "" && cfg.BaseURLEnv != "" {
		baseURL = os.Getenv(cfg.BaseURLEnv)
	}

	config := &openai.ChatModelConfig{
		APIKey:     apiKey,
		Model:      modelName,
		HTTPClient: httpClient,
	}
	// GPT-5+ models require max_completion_tokens; older models use max_tokens.
	if strings.HasPrefix(modelName, "gpt-5") {
		config.MaxCompletionTokens = &cfg.MaxTokens
	} else {
		config.MaxTokens = &cfg.MaxTokens
	}
	if baseURL != "" {
		config.BaseURL = baseURL
	}

	return openai.NewChatModel(ctx, config)
}

func newOpenAICompatModel(ctx context.Context, modelName string, cfg *ProviderConfig) (model.ChatModel, error) {
	baseURLEnv := "OPENAI_COMPAT_BASE_URL"
	if cfg.BaseURLEnv != "" {
		baseURLEnv = cfg.BaseURLEnv
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = os.Getenv(baseURLEnv)
	}
	if cfg.BaseURL == "" {
		return nil, fmt.Errorf("base URL is required for openai-compat provider (set %s)", baseURLEnv)
	}

	apiKey := cfg.APIKey
	apiKeyEnv := "OPENAI_COMPAT_API_KEY"
	if cfg.APIKeyEnv != "" {
		apiKeyEnv = cfg.APIKeyEnv
	}
	if apiKey == "" {
		apiKey = os.Getenv(apiKeyEnv)
	}
	config := &openai.ChatModelConfig{
		APIKey:     apiKey,
		Model:      modelName,
		BaseURL:    cfg.BaseURL,
		MaxTokens:  &cfg.MaxTokens,
		HTTPClient: httpClient,
	}

	return openai.NewChatModel(ctx, config)
}

func newGoogleModel(ctx context.Context, modelName string, cfg *ProviderConfig) (model.ChatModel, error) {
	apiKey := cfg.APIKey
	envName := "GOOGLE_API_KEY"
	if cfg.APIKeyEnv != "" {
		envName = cfg.APIKeyEnv
	}
	if apiKey == "" {
		apiKey = os.Getenv(envName)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("%s is required for google provider", envName)
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     apiKey,
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}

	config := &gemini.Config{
		Client:    client,
		Model:     modelName,
		MaxTokens: &cfg.MaxTokens,
	}

	return gemini.NewChatModel(ctx, config)
}
