package agentd

func codexStopRecipe() terminalStopRecipe {
	return terminalStopRecipe{name: "codex", signalKeys: []string{"C-c", "C-c", "C-c", "C-c"}, recordExitReason: true}
}
