package model

import "time"

// ContextUsage is the last native context-window observation, not cumulative
// billing usage. Missing evidence remains unknown rather than appearing empty.
type ContextUsage struct {
	ModelWindow     int64
	EffectiveWindow int64
	NativePercent   float64
	UsedPercent     float64
	ObservedAt      time.Time
}
