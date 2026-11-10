package agentd

// ResetNodeUpdateScriptsForTest drops the update-scripts service so the next
// request starts one with a fresh status cache.
func ResetNodeUpdateScriptsForTest() func() {
	stopNodeUpdateScripts()
	return stopNodeUpdateScripts
}
