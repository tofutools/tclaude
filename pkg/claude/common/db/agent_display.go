package db

// AgentDisplayTitle applies the canonical precedence for an agent's readable
// name from already-loaded identity parts. Keeping the pure rule beside the DB
// row types lets transactional writers and higher-level listing surfaces share
// it without creating an import cycle.
func AgentDisplayTitle(row *ConvIndexRow, pendingName string) string {
	if row != nil && row.CustomTitle != "" {
		return row.CustomTitle
	}
	if pendingName != "" {
		return pendingName
	}
	if row != nil && row.Summary != "" {
		return row.Summary
	}
	if row != nil {
		return row.FirstPrompt
	}
	return ""
}
