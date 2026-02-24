package message

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMessageJSONRoundTrip(t *testing.T) {
	original := Message{
		ID:        "msg-1",
		RequestID: "req-1",
		From:      "conductor",
		To:        "developer",
		Type:      TypeTaskAssignment,
		Parts: []Part{
			TextPart{Text: "Write tests"},
			DataPart{Data: map[string]any{"lang": "go"}},
		},
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var restored Message
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if restored.ID != original.ID {
		t.Errorf("ID: got %q, want %q", restored.ID, original.ID)
	}
	if len(restored.Parts) != 2 {
		t.Fatalf("Parts: got %d, want 2", len(restored.Parts))
	}

	tp, ok := restored.Parts[0].(TextPart)
	if !ok {
		t.Fatalf("Part 0: expected TextPart, got %T", restored.Parts[0])
	}
	if tp.Text != "Write tests" {
		t.Errorf("TextPart: got %q", tp.Text)
	}

	dp, ok := restored.Parts[1].(DataPart)
	if !ok {
		t.Fatalf("Part 1: expected DataPart, got %T", restored.Parts[1])
	}
	if dp.Data["lang"] != "go" {
		t.Errorf("DataPart lang: got %v", dp.Data["lang"])
	}
}
