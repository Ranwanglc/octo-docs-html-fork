package storage

import (
	"encoding/json"
	"maps"
)

// DocMeta serializes with Extra fields flattened alongside the known fields, to
// match the upstream shape where metadata is an open object ({title, slug,
// versions, ...extra}). This keeps round-tripping byte-compatible with stored
// records written by the TypeScript server.

// MarshalJSON flattens Extra into the top-level object.
func (m DocMeta) MarshalJSON() ([]byte, error) {
	out := map[string]any{}
	maps.Copy(out, m.Extra)
	out["slug"] = m.Slug
	out["title"] = m.Title
	out["versions"] = m.Versions
	return json.Marshal(out)
}

// UnmarshalJSON extracts the known fields and collects the rest into Extra.
func (m *DocMeta) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if v, ok := raw["slug"]; ok {
		if err := json.Unmarshal(v, &m.Slug); err != nil {
			return err
		}
	}
	if v, ok := raw["title"]; ok {
		if err := json.Unmarshal(v, &m.Title); err != nil {
			return err
		}
	}
	if v, ok := raw["versions"]; ok {
		if err := json.Unmarshal(v, &m.Versions); err != nil {
			return err
		}
	}
	delete(raw, "slug")
	delete(raw, "title")
	delete(raw, "versions")
	if len(raw) > 0 {
		m.Extra = make(map[string]any, len(raw))
		for k, v := range raw {
			var val any
			if err := json.Unmarshal(v, &val); err != nil {
				return err
			}
			m.Extra[k] = val
		}
	}
	return nil
}
