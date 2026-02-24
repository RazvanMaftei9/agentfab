package routing

import (
	"context"
	"log/slog"

	"github.com/cloudwego/eino/components/model"
	"github.com/razvanmaftei/agentfab/internal/runtime"
)

// AdaptiveRouter wraps a base model factory with tier-based dynamic routing.
// It starts with the cheapest model and escalates on failure.
type AdaptiveRouter struct {
	Base    func(ctx context.Context, modelID string) (model.ChatModel, error)
	Tiers   map[string][]string // agent name → [cheap, ..., expensive] model IDs
	tracker *tracker
}

// NewAdaptiveRouter creates a new AdaptiveRouter.
func NewAdaptiveRouter(base func(ctx context.Context, modelID string) (model.ChatModel, error), tiers map[string][]string) *AdaptiveRouter {
	return &AdaptiveRouter{
		Base:    base,
		Tiers:   tiers,
		tracker: newTracker(),
	}
}

// ModelFactory returns a factory function suitable for passing to conductor.New().
func (r *AdaptiveRouter) ModelFactory() func(ctx context.Context, modelID string) (model.ChatModel, error) {
	return func(ctx context.Context, defaultModel string) (model.ChatModel, error) {
		agent := runtime.AgentNameFrom(ctx)
		taskID := runtime.TaskIDFrom(ctx)

		tiers, ok := r.Tiers[agent]
		if !ok || len(tiers) == 0 {
			return r.Base(ctx, defaultModel)
		}

		tier := r.tracker.currentTier(agent, taskID)
		if tier >= len(tiers) {
			tier = len(tiers) - 1
		}
		selectedModel := tiers[tier]

		slog.Debug("adaptive routing: selected model",
			"agent", agent, "task", taskID, "tier", tier, "model", selectedModel)

		return r.Base(ctx, selectedModel)
	}
}

// Escalate advances the tier for (agent, taskID). Returns false if already at max.
func (r *AdaptiveRouter) Escalate(agent, taskID string) bool {
	tiers, ok := r.Tiers[agent]
	if !ok || len(tiers) == 0 {
		return false
	}
	maxTier := len(tiers) - 1
	newTier, escalated := r.tracker.escalate(agent, taskID, maxTier)
	if escalated {
		slog.Info("adaptive routing: escalated",
			"agent", agent, "task", taskID, "newTier", newTier, "model", tiers[newTier])
	}
	return escalated
}

// RecordOutcome records success/failure for the current tier.
func (r *AdaptiveRouter) RecordOutcome(agent, taskID string, success bool) {
	tiers, ok := r.Tiers[agent]
	if !ok || len(tiers) == 0 {
		return
	}
	tier := r.tracker.currentTier(agent, taskID)
	if tier >= len(tiers) {
		tier = len(tiers) - 1
	}
	r.tracker.recordOutcome(agent, tiers[tier], tier, success)
}

// Report returns aggregate tier usage and success rates.
func (r *AdaptiveRouter) Report() []TierStat {
	return r.tracker.report()
}
