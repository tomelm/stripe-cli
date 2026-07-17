package verification

import (
	"encoding/json"
	"sort"
)

// MarshalDeterministic validates set and returns a stable compact JSON
// encoding. Results are ordered by result ID and evidence by key. The method
// operates on a copy and never reorders caller-owned slices.
func (set ResultSet) MarshalDeterministic() ([]byte, error) {
	if err := set.Validate(); err != nil {
		return nil, err
	}

	normalized := ResultSet{
		SchemaVersion: set.SchemaVersion,
		Results:       make([]Result, len(set.Results)),
	}
	copy(normalized.Results, set.Results)
	for i := range normalized.Results {
		normalized.Results[i].Evidence = append([]Evidence(nil), normalized.Results[i].Evidence...)
		if len(normalized.Results[i].Evidence) == 0 {
			normalized.Results[i].Evidence = nil
			continue
		}
		sort.Slice(normalized.Results[i].Evidence, func(left, right int) bool {
			return normalized.Results[i].Evidence[left].Key < normalized.Results[i].Evidence[right].Key
		})
	}
	sort.Slice(normalized.Results, func(left, right int) bool {
		return normalized.Results[left].ID < normalized.Results[right].ID
	})
	return json.Marshal(normalized)
}
