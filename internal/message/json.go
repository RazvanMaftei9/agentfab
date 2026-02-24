package message

import (
	"encoding/json"
	"fmt"
)

// partJSON is the JSON envelope for Part serialization.
type partJSON struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// MarshalJSON implements custom JSON marshaling for Message to handle Part interface.
func (m Message) MarshalJSON() ([]byte, error) {
	type Alias Message

	parts := make([]partJSON, len(m.Parts))
	for i, p := range m.Parts {
		data, err := json.Marshal(p)
		if err != nil {
			return nil, fmt.Errorf("marshal part %d: %w", i, err)
		}
		parts[i] = partJSON{Type: p.partType(), Data: data}
	}

	return json.Marshal(struct {
		Alias
		Parts []partJSON `json:"parts"`
	}{
		Alias: Alias(m),
		Parts: parts,
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for Message to handle Part interface.
func (m *Message) UnmarshalJSON(data []byte) error {
	type Alias Message

	aux := struct {
		Alias
		Parts []partJSON `json:"parts"`
	}{}

	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	*m = Message(aux.Alias)
	m.Parts = make([]Part, len(aux.Parts))

	for i, p := range aux.Parts {
		switch p.Type {
		case "text":
			var tp TextPart
			if err := json.Unmarshal(p.Data, &tp); err != nil {
				return fmt.Errorf("unmarshal text part %d: %w", i, err)
			}
			m.Parts[i] = tp
		case "file":
			var fp FilePart
			if err := json.Unmarshal(p.Data, &fp); err != nil {
				return fmt.Errorf("unmarshal file part %d: %w", i, err)
			}
			m.Parts[i] = fp
		case "data":
			var dp DataPart
			if err := json.Unmarshal(p.Data, &dp); err != nil {
				return fmt.Errorf("unmarshal data part %d: %w", i, err)
			}
			m.Parts[i] = dp
		default:
			return fmt.Errorf("unknown part type %q at index %d", p.Type, i)
		}
	}

	return nil
}
