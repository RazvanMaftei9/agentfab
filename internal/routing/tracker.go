package routing

import "sync"

// TierStat tracks success/failure counts for a specific agent+model combination.
type TierStat struct {
	Agent    string
	Model    string
	Tier     int
	Success  int
	Failure  int
	Total    int
}

// tracker manages per-(agent, taskID) tier state and aggregate statistics.
type tracker struct {
	mu    sync.RWMutex
	state map[string]int      // "agent:taskID" → current tier index
	stats map[string]*tierAcc // "agent:model" → success/failure counts
}

type tierAcc struct {
	agent   string
	model   string
	tier    int
	success int
	failure int
}

func newTracker() *tracker {
	return &tracker{
		state: make(map[string]int),
		stats: make(map[string]*tierAcc),
	}
}

func tierKey(agent, taskID string) string {
	return agent + ":" + taskID
}

func statKey(agent, model string) string {
	return agent + ":" + model
}

// currentTier returns the current tier index for (agent, taskID). Returns 0 if not set.
func (t *tracker) currentTier(agent, taskID string) int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state[tierKey(agent, taskID)]
}

// escalate increments the tier for (agent, taskID). Returns the new tier and true if escalation happened.
// Returns (current, false) if already at maxTier.
func (t *tracker) escalate(agent, taskID string, maxTier int) (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := tierKey(agent, taskID)
	cur := t.state[key]
	if cur >= maxTier {
		return cur, false
	}
	t.state[key] = cur + 1
	return cur + 1, true
}

// recordOutcome records a success or failure for the current tier.
func (t *tracker) recordOutcome(agent, model string, tier int, success bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := statKey(agent, model)
	acc, ok := t.stats[key]
	if !ok {
		acc = &tierAcc{agent: agent, model: model, tier: tier}
		t.stats[key] = acc
	}
	if success {
		acc.success++
	} else {
		acc.failure++
	}
}

// report returns aggregate tier usage and success rates.
func (t *tracker) report() []TierStat {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]TierStat, 0, len(t.stats))
	for _, acc := range t.stats {
		result = append(result, TierStat{
			Agent:   acc.agent,
			Model:   acc.model,
			Tier:    acc.tier,
			Success: acc.success,
			Failure: acc.failure,
			Total:   acc.success + acc.failure,
		})
	}
	return result
}
