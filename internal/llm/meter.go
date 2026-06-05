package llm

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

const (
	// defaultCallTimeout was raised from 25m to 35m on 2026-05-27 to
	// accommodate the us-energy-grid full-US run, where viz-engineer
	// streams a ~30 KB HTML+JS bundle on Opus 4.7. Opus output rate is
	// roughly half Sonnet's, so a single viz call needs ~12 min of
	// streaming time alone; the original 25 min left only ~12 min of
	// slack for thinking before SSE timeout. 35 min gives ~22 min of
	// slack which is the right margin for code-gen tasks on Opus.
	// Analytical agents on Sonnet finish in 30-90s; the longer timeout
	// is only ever hit by the long-tail.
	defaultCallTimeout = 35 * time.Minute
	retryCallTimeout   = 12 * time.Minute // shorter timeout for retry attempts (proportional to default)

	// maxRetries was raised from 3 to 6 on 2026-05-27 as the lightweight
	// approximation of roadmap item A (per-call retry-with-checkpoint).
	// The full checkpoint mechanism — persist tool-loop state across LLM
	// failures so a stalled SSE stream doesn't discard 30-60 min of work —
	// is deferred; in its place this bump gives a single inference call
	// 35m + 6×12m = 107m of total patience before escalating to the task
	// level. Combined with viz-engineer's task_timeout of 60m (raise to
	// 120m for full-US), this means an SSE stall blocks a retry rather
	// than wiping the task. Watch for: cumulative retries on a single
	// call where every attempt streams ~zero tokens (the diagnostic for
	// a deeper provider issue that no amount of retry will fix).
	maxRetries     = 6
	baseRetryDelay = 2 * time.Second
)

// RetryCallback is called before each retry attempt.
type RetryCallback func(attempt, maxAttempts int, err error)

// MeteredModel wraps a ChatModel to record token usage and retry transient errors.
type MeteredModel struct {
	Model       model.BaseChatModel
	AgentName   string
	ModelID     string
	Meter       runtime.Meter
	OnChunk     ChunkCallback  // Optional streaming progress callback.
	OnRetry     RetryCallback  // Optional callback fired before each retry attempt.
	DebugLog    *DebugStore    // Optional debug store for per-agent request/response logging.
	CallTimeout time.Duration  // Per-call timeout. Zero uses defaultCallTimeout (35m).
	Options     []model.Option // Provider-specific options (e.g., prompt caching).
}

// Generate calls the model via streaming with retry and records token usage.
func (m *MeteredModel) Generate(ctx context.Context, input []*schema.Message) (*schema.Message, error) {
	timeout := m.CallTimeout
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if ctx.Err() != nil {
				return nil, lastErr
			}
			// Exponential backoff: 2s, 4s, 8s + jitter (up to 25% of delay).
			delay := baseRetryDelay * (1 << (attempt - 1))
			jitter := time.Duration(rand.Int63n(int64(delay) / 4))

			slog.Warn("model call failed, retrying",
				"agent", m.AgentName, "attempt", attempt, "max", maxRetries,
				"delay", delay+jitter, "error", lastErr)

			if m.OnRetry != nil {
				m.OnRetry(attempt, maxRetries, lastErr)
			}

			select {
			case <-time.After(delay + jitter):
			case <-ctx.Done():
				return nil, lastErr
			}
		}

		callTimeout := timeout
		if attempt > 0 {
			callTimeout = retryCallTimeout // 12min for retries vs 35min first attempt
		}
		callCtx, cancel := context.WithTimeout(ctx, callTimeout)

		start := time.Now()
		resp, err := StreamCollectWithCallback(callCtx, m.Model, input, m.OnChunk, m.Options...)
		duration := time.Since(start)
		cancel()

		if err != nil {
			if isTransientError(err) && attempt < maxRetries {
				lastErr = err
				continue
			}
			return nil, err
		}

		record := runtime.LLMCallRecord{
			AgentName: m.AgentName,
			RequestID: runtime.RequestIDFrom(ctx),
			TaskID:    runtime.TaskIDFrom(ctx),
			Model:     m.ModelID,
			Duration:  duration,
			Timestamp: start,
		}

		if resp.ResponseMeta != nil {
			if resp.ResponseMeta.Usage != nil {
				record.InputTokens = int64(resp.ResponseMeta.Usage.PromptTokens)
				record.OutputTokens = int64(resp.ResponseMeta.Usage.CompletionTokens)
				if resp.ResponseMeta.Usage.PromptTokenDetails.CachedTokens > 0 {
					record.CacheReadTokens = int64(resp.ResponseMeta.Usage.PromptTokenDetails.CachedTokens)
				}
			}
			if resp.ResponseMeta.FinishReason != "" {
				record.FinishReason = resp.ResponseMeta.FinishReason
			}
		}

		m.Meter.Record(ctx, record)

		// Log prompt_tokens separately from input_tokens. eino-ext sums
		// InputTokens + CacheReadInputTokens + CacheCreationInputTokens
		// into PromptTokens; logging both helps diagnose caching.
		promptTokensRaw := 0
		cachedTokensRaw := 0
		if resp.ResponseMeta != nil && resp.ResponseMeta.Usage != nil {
			promptTokensRaw = resp.ResponseMeta.Usage.PromptTokens
			cachedTokensRaw = resp.ResponseMeta.Usage.PromptTokenDetails.CachedTokens
		}
		slog.Info("model call",
			"agent", m.AgentName,
			"model", m.ModelID,
			"request_id", record.RequestID,
			"task_id", record.TaskID,
			"input_tokens", record.InputTokens,
			"output_tokens", record.OutputTokens,
			"cache_read_tokens", record.CacheReadTokens,
			"prompt_tokens_raw", promptTokensRaw,
			"cached_tokens_raw", cachedTokensRaw,
			"duration_ms", duration.Milliseconds(),
			"finish_reason", record.FinishReason,
		)

		if m.DebugLog != nil {
			m.DebugLog.Log(m.AgentName, m.ModelID, duration, input, resp)
		}

		return resp, nil
	}
	return nil, lastErr
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := err.Error()
	for _, code := range []string{"429", "500", "502", "503", "504"} {
		if strings.Contains(s, "status "+code) || strings.Contains(s, "Status: "+code) {
			return true
		}
	}
	transientPatterns := []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"EOF",
		"stream recv:",
		"empty stream response",
	}
	for _, p := range transientPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
