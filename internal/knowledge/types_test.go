package knowledge

import (
	"testing"
	"time"
)

func TestNodeIsStale(t *testing.T) {
	n := &Node{
		UpdatedAt: time.Now().Add(-48 * time.Hour), // 2 days ago
		TTLDays:   1,                               // expires after 1 day
	}
	if !n.IsStale() {
		t.Error("node should be stale (2 days old, TTL 1 day)")
	}
}

func TestNodeIsStaleNoTTL(t *testing.T) {
	n := &Node{
		UpdatedAt: time.Now().Add(-365 * 24 * time.Hour), // 1 year ago
		TTLDays:   0,                                     // no expiry
	}
	if n.IsStale() {
		t.Error("node with TTL=0 should never be stale")
	}
}

func TestNodeIsStaleNotExpired(t *testing.T) {
	n := &Node{
		UpdatedAt: time.Now().Add(-12 * time.Hour), // 12 hours ago
		TTLDays:   1,                               // expires after 1 day
	}
	if n.IsStale() {
		t.Error("node should not be stale yet (12h old, TTL 1 day)")
	}
}

func TestNodeIsStaleNegativeTTL(t *testing.T) {
	n := &Node{
		UpdatedAt: time.Now().Add(-48 * time.Hour),
		TTLDays:   -1,
	}
	if n.IsStale() {
		t.Error("node with negative TTL should never be stale")
	}
}

func TestNodeHasTag(t *testing.T) {
	n := &Node{Tags: []string{"api", "Decision", "design-system"}}

	if !n.HasTag("decision") {
		t.Error("HasTag should be case-insensitive")
	}
	if !n.HasTag("Decision") {
		t.Error("HasTag should match exact case")
	}
	if !n.HasTag("API") {
		t.Error("HasTag should find API (case-insensitive)")
	}
	if n.HasTag("nonexistent") {
		t.Error("HasTag should return false for missing tag")
	}

	empty := &Node{}
	if empty.HasTag("anything") {
		t.Error("HasTag on empty tags should return false")
	}
}
