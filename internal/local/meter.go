package local

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/razvanmaftei/agentfab/internal/runtime"
)

var _ runtime.ExtendedMeter = (*Meter)(nil)

type Meter struct {
	mu      sync.RWMutex
	records map[string][]runtime.LLMCallRecord // agentName -> records
	budgets map[string]runtime.Budget          // agentName -> budget
}

func NewMeter() *Meter {
	return &Meter{
		records: make(map[string][]runtime.LLMCallRecord),
		budgets: make(map[string]runtime.Budget),
	}
}

func (m *Meter) Record(_ context.Context, record runtime.LLMCallRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.records[record.AgentName] = append(m.records[record.AgentName], record)
	return nil
}

func (m *Meter) Usage(_ context.Context, agentName string) (runtime.UsageSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.summarize(m.records[agentName]), nil
}

func (m *Meter) AggregateUsage(_ context.Context) (runtime.UsageSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []runtime.LLMCallRecord
	for _, recs := range m.records {
		all = append(all, recs...)
	}
	return m.summarize(all), nil
}

func (m *Meter) CheckBudget(_ context.Context, agentName string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	budget, ok := m.budgets[agentName]
	if !ok {
		return nil // no budget set
	}

	summary := m.summarize(m.records[agentName])

	if budget.MaxInputTokens > 0 && summary.InputTokens > budget.MaxInputTokens {
		return fmt.Errorf("agent %q exceeded input token budget (%d/%d)", agentName, summary.InputTokens, budget.MaxInputTokens)
	}
	if budget.MaxOutputTokens > 0 && summary.OutputTokens > budget.MaxOutputTokens {
		return fmt.Errorf("agent %q exceeded output token budget (%d/%d)", agentName, summary.OutputTokens, budget.MaxOutputTokens)
	}
	if budget.MaxTotalTokens > 0 && summary.TotalTokens > budget.MaxTotalTokens {
		return fmt.Errorf("agent %q exceeded total token budget (%d/%d)", agentName, summary.TotalTokens, budget.MaxTotalTokens)
	}
	return nil
}

func (m *Meter) SetBudget(_ context.Context, agentName string, budget runtime.Budget) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.budgets[agentName] = budget
	return nil
}

func (m *Meter) ModelUsage(_ context.Context) []runtime.ModelUsage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	byModel := map[string]*runtime.ModelUsage{}
	for _, recs := range m.records {
		for _, r := range recs {
			mu := byModel[r.Model]
			if mu == nil {
				mu = &runtime.ModelUsage{Model: r.Model}
				byModel[r.Model] = mu
			}
			mu.InputTokens += r.InputTokens
			mu.OutputTokens += r.OutputTokens
			mu.CacheReadTokens += r.CacheReadTokens
			mu.TotalTokens += r.InputTokens + r.OutputTokens
			mu.TotalCalls++
		}
	}
	result := make([]runtime.ModelUsage, 0, len(byModel))
	for _, mu := range byModel {
		result = append(result, *mu)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Model < result[j].Model
	})
	return result
}

func (m *Meter) RequestUsage(_ context.Context, requestID string) runtime.UsageSummary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var recs []runtime.LLMCallRecord
	for _, agentRecs := range m.records {
		for _, r := range agentRecs {
			if r.RequestID == requestID {
				recs = append(recs, r)
			}
		}
	}
	return m.summarize(recs)
}

func (m *Meter) AllAgentUsage(_ context.Context) map[string]runtime.UsageSummary {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]runtime.UsageSummary, len(m.records))
	for name, recs := range m.records {
		result[name] = m.summarize(recs)
	}
	return result
}

func (m *Meter) AllRecords(_ context.Context) []runtime.LLMCallRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var all []runtime.LLMCallRecord
	for _, recs := range m.records {
		all = append(all, recs...)
	}
	return all
}

func (m *Meter) summarize(records []runtime.LLMCallRecord) runtime.UsageSummary {
	var s runtime.UsageSummary
	for _, r := range records {
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
		s.CacheReadTokens += r.CacheReadTokens
		s.TotalTokens += r.InputTokens + r.OutputTokens
		s.TotalCalls++
		s.TotalTime += r.Duration
	}
	return s
}
