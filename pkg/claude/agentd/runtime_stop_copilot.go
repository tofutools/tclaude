package agentd

func copilotStopRecipe() terminalStopRecipe {
	return terminalStopRecipe{name: "copilot", signalKeys: []string{"C-c", "C-c", "C-c", "C-c"}}
}
