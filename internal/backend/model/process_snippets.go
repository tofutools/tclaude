package model

import (
	"encoding/json"
	"time"
)

// ProcessSnippet is reusable authoring material, never admitted executable work.
type ProcessSnippet struct {
	ID        string
	Name      string
	Revision  Revision
	CreatedAt time.Time
	UpdatedAt time.Time
	Deleted   bool
	Available bool
	Selection json.RawMessage `json:",omitempty"`
}

type ProcessSelection struct {
	EdgeLabels []EditorEdgeLabel             `json:"edgeLabels,omitempty"`
	Version    int                           `json:"version"`
	Nodes      []WorkNode                    `json:"nodes"`
	Edges      []WorkEdge                    `json:"edges"`
	Positions  map[WorkNodeID]EditorPosition `json:"positions"`
}
