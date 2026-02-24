package knowledge

import (
	"strings"
	"time"
)

type Graph struct {
	Version   int              `json:"version"`
	UpdatedAt time.Time        `json:"updated_at"`
	Nodes     map[string]*Node `json:"nodes"`
	Edges     []*Edge          `json:"edges"`
}

// Node is a knowledge unit owned by a specific agent.
// ID convention: {agent}/{kebab-slug} (e.g., "agent-name/kebab-slug").
type Node struct {
	ID          string    `json:"id"`
	Agent       string    `json:"agent"`
	Title       string    `json:"title"`
	FilePath    string    `json:"file_path"` // relative to agent tier root, e.g. docs/api-design.md
	Tags        []string  `json:"tags,omitempty"`
	RequestID   string    `json:"request_id"`
	RequestName string    `json:"request_name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Summary     string    `json:"summary"`              // one-paragraph summary for context injection
	Confidence  float64   `json:"confidence,omitempty"` // 0-1, LLM-assigned
	Source      string    `json:"source,omitempty"`     // "task_result", "inferred", "user_provided"
	TTLDays     int       `json:"ttl_days,omitempty"`   // 0 = no expiry
	Hits        int64     `json:"hits,omitempty"`        // lookup hit counter (in-memory, persisted on save)
	LastHitAt   time.Time `json:"last_hit_at,omitempty"` // last time this node was returned by a lookup
}

// IsStale returns true if the node has a TTL and has exceeded it.
func (n *Node) IsStale() bool {
	if n.TTLDays <= 0 {
		return false
	}
	return time.Since(n.UpdatedAt) > time.Duration(n.TTLDays)*24*time.Hour
}

// IsCold returns true if the node has fewer than minHits and was last accessed more than coldDays ago.
func (n *Node) IsCold(minHits int64, coldDays int) bool {
	if n.Hits >= minHits {
		return false
	}
	threshold := time.Duration(coldDays) * 24 * time.Hour
	if !n.LastHitAt.IsZero() {
		return time.Since(n.LastHitAt) > threshold
	}
	return time.Since(n.CreatedAt) > threshold // never accessed, use CreatedAt
}

func (n *Node) RecordHit() {
	n.Hits++
	n.LastHitAt = time.Now()
}

func (n *Node) HasTag(tag string) bool {
	tag = strings.ToLower(tag)
	for _, t := range n.Tags {
		if strings.ToLower(t) == tag {
			return true
		}
	}
	return false
}

// DomainTags returns tags excluding "decision" itself, used for conflict detection.
func (n *Node) DomainTags() []string {
	var tags []string
	for _, t := range n.Tags {
		lower := strings.ToLower(t)
		if lower != "decision" {
			tags = append(tags, lower)
		}
	}
	return tags
}

type Edge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Relation string `json:"relation"` // depends_on, implements, related_to, supersedes, refines
}

type Manifest struct {
	Nodes []ManifestNode `json:"nodes"`
	Edges []Edge         `json:"edges"`
}

// PruneOpts controls knowledge graph pruning behavior.
type PruneOpts struct {
	ConfidenceFloor float64 // evict nodes where 0 < confidence < floor (default 0.2); confidence=0 (unset) is NOT evicted
	MaxNodes        int     // node cap (default 200); 0 = use default
}

func (o PruneOpts) confidenceFloor() float64 {
	if o.ConfidenceFloor <= 0 {
		return 0.2
	}
	return o.ConfidenceFloor
}

func (o PruneOpts) maxNodes() int {
	if o.MaxNodes <= 0 {
		return 200
	}
	return o.MaxNodes
}

type ManifestNode struct {
	ID         string   `json:"id"`
	Agent      string   `json:"agent"`
	Title      string   `json:"title"`
	Summary    string   `json:"summary"`
	Content    string   `json:"content"` // full markdown doc to write to disk
	Tags       []string `json:"tags,omitempty"`
	Confidence float64  `json:"confidence,omitempty"` // 0-1, LLM-assigned
	Source     string   `json:"source,omitempty"`     // "task_result", "inferred", "user_provided"
	TTLDays    int      `json:"ttl_days,omitempty"`   // 0 = no expiry
}

// ColdStorageOpts controls cold storage eviction and retention.
type ColdStorageOpts struct {
	MinHits       int64 // minimum hits to stay active (default 3)
	ColdDays      int   // days since last access to be considered cold (default 30)
	RetentionDays int   // days to retain cold nodes before permanent purge (default 1095 = 3 years)
}

func (o ColdStorageOpts) minHits() int64 {
	if o.MinHits <= 0 {
		return 3
	}
	return o.MinHits
}

func (o ColdStorageOpts) coldDays() int {
	if o.ColdDays <= 0 {
		return 30
	}
	return o.ColdDays
}

func (o ColdStorageOpts) retentionDays() int {
	if o.RetentionDays <= 0 {
		return 1095
	}
	return o.RetentionDays
}

// CurationOpts controls LLM-based knowledge graph curation.
type CurationOpts struct {
	Threshold int            // minimum node count to trigger curation (default 50)
	ColdOpts  ColdStorageOpts // cold storage eviction options applied during curation
}

func (o CurationOpts) threshold() int {
	if o.Threshold <= 0 {
		return 50
	}
	return o.Threshold
}
