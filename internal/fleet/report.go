package fleet

import "time"

// WarmItem is one module a cache sync resolved and pre-cached.
type WarmItem struct {
	Module  string `json:"module"`
	Version string `json:"version,omitempty"`
	Bytes   int64  `json:"bytes"`
	Source  string `json:"source,omitempty"` // neighbour host that served it; empty for upstream
	Err     string `json:"error,omitempty"`  // non-empty when the module could not be warmed
}

// WarmReport is the outcome of the most recent fleet cache sync. The zero
// value means no pass has run yet.
type WarmReport struct {
	StartedAt  time.Time  `json:"startedAt"`
	DurationMS int64      `json:"durationMs"`
	NextRun    time.Time  `json:"nextRun"`
	Modules    int        `json:"modules"` // modules considered
	Fetched    int        `json:"fetched"` // modules whose files were downloaded
	Bytes      int64      `json:"bytes"`   // upstream or peer bytes pulled
	Failed     int        `json:"failed"`  // modules that could not be warmed
	Items      []WarmItem `json:"items"`
}
