package agentd

func claudeStopRecipe() terminalStopRecipe {
	return terminalStopRecipe{name: "claude", signalKeys: []string{"Escape", "C-c", "C-c", "C-c", "C-c"}}
}
