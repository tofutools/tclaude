package state

func Clone(st State) State {
	out := st
	if st.TemplateDivergence != nil {
		divergence := *st.TemplateDivergence
		out.TemplateDivergence = &divergence
	}
	out.Nodes = make(map[string]NodeState, len(st.Nodes))
	for key, value := range st.Nodes {
		if value.ActiveAttempt != nil {
			attempt := *value.ActiveAttempt
			value.ActiveAttempt = &attempt
		}
		value.Decisions = append([]DecisionRecord(nil), value.Decisions...)
		out.Nodes[key] = value
	}
	out.OutstandingCommands = make(map[string]OutstandingCommand, len(st.OutstandingCommands))
	for key, value := range st.OutstandingCommands {
		value.Payload = append([]byte(nil), value.Payload...)
		out.OutstandingCommands[key] = value
	}
	out.Waits = make(map[string]WaitRecord, len(st.Waits))
	for key, value := range st.Waits {
		out.Waits[key] = value
	}
	out.Timers = make(map[string]TimerRecord, len(st.Timers))
	for key, value := range st.Timers {
		out.Timers[key] = value
	}
	out.AdminRecords = append([]AdminRecord(nil), st.AdminRecords...)
	return out
}
