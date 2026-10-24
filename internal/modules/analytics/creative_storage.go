package analytics

// storageRow is one kind's stored Creative Studio files in one state.
type storageRow struct {
	Kind  string `json:"kind"`
	Bytes int64  `json:"bytes"`
	Files int    `json:"files"`
}

// creativeStorage shapes the platform usage block: totals, then per kind for
// drafts (they still expire) and kept files (approved versions).
func creativeStorage(drafts, kept []storageRow) map[string]any {
	state := func(rows []storageRow) map[string]any {
		var bytes int64
		var files int
		byKind := map[string]any{}
		for _, r := range rows {
			bytes += r.Bytes
			files += r.Files
			byKind[r.Kind] = map[string]any{"bytes": r.Bytes, "files": r.Files}
		}
		return map[string]any{"bytes": bytes, "files": files, "by_kind": byKind}
	}
	d, k := state(drafts), state(kept)
	return map[string]any{
		"bytes":  d["bytes"].(int64) + k["bytes"].(int64),
		"files":  d["files"].(int) + k["files"].(int),
		"drafts": d,
		"kept":   k,
	}
}
