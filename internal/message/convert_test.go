package message

import (
	"testing"
	"time"
)

func TestProtoRoundTrip(t *testing.T) {
	original := &Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "architect",
		Type:      TypeTaskAssignment,
		Parts: []Part{
			TextPart{Text: "Design the auth system"},
			FilePart{URI: "artifacts/conductor/req-1/spec.md", MimeType: "text/markdown", Name: "spec.md"},
			DataPart{Data: map[string]any{"priority": "high", "count": float64(3)}},
		},
		Metadata:   map[string]string{"trace": "abc"},
		TokenUsage: &TokenUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		Timestamp:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	pb := ToProto(original)
	if pb == nil {
		t.Fatal("ToProto returned nil")
	}

	restored := FromProto(pb)
	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if restored.Type != original.Type {
		t.Errorf("Type: got %q, want %q", restored.Type, original.Type)
	}
	if len(restored.Parts) != len(original.Parts) {
		t.Fatalf("Parts: got %d, want %d", len(restored.Parts), len(original.Parts))
	}

	// Check text part.
	tp, ok := restored.Parts[0].(TextPart)
	if !ok {
		t.Fatalf("Part 0: expected TextPart, got %T", restored.Parts[0])
	}
	if tp.Text != "Design the auth system" {
		t.Errorf("TextPart: got %q", tp.Text)
	}

	// Check file part.
	fp, ok := restored.Parts[1].(FilePart)
	if !ok {
		t.Fatalf("Part 1: expected FilePart, got %T", restored.Parts[1])
	}
	if fp.URI != "artifacts/conductor/req-1/spec.md" {
		t.Errorf("FilePart URI: got %q", fp.URI)
	}

	// Check data part.
	dp, ok := restored.Parts[2].(DataPart)
	if !ok {
		t.Fatalf("Part 2: expected DataPart, got %T", restored.Parts[2])
	}
	if dp.Data["priority"] != "high" {
		t.Errorf("DataPart priority: got %v", dp.Data["priority"])
	}

	// Check token usage.
	if restored.TokenUsage.InputTokens != 100 {
		t.Errorf("InputTokens: got %d", restored.TokenUsage.InputTokens)
	}

	// Check metadata.
	if restored.Metadata["trace"] != "abc" {
		t.Errorf("Metadata trace: got %q", restored.Metadata["trace"])
	}
}

func TestProtoNilHandling(t *testing.T) {
	if ToProto(nil) != nil {
		t.Error("ToProto(nil) should return nil")
	}
	if FromProto(nil) != nil {
		t.Error("FromProto(nil) should return nil")
	}
}
