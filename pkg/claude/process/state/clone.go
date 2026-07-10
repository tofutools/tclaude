package state

func Clone(st State) State {
	out := st
	if st.Pause != nil {
		pause := *st.Pause
		out.Pause = &pause
	}
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
		if value.PendingFeedback != nil {
			feedback := *value.PendingFeedback
			value.PendingFeedback = &feedback
		}
		if value.BlockResolution != nil {
			resolution := *value.BlockResolution
			value.BlockResolution = &resolution
		}
		value.Children = append([]string(nil), value.Children...)
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
	out.Obligations = make(map[string]ObligationRecord, len(st.Obligations))
	for key, value := range st.Obligations {
		value.AvailableActions = append([]string(nil), value.AvailableActions...)
		out.Obligations[key] = value
	}
	out.Contacts = make(map[string]ContactState, len(st.Contacts))
	for key, value := range st.Contacts {
		out.Contacts[key] = value
	}
	out.AdminRecords = append([]AdminRecord(nil), st.AdminRecords...)
	for i := range out.AdminRecords {
		if out.AdminRecords[i].Resolution != nil {
			resolution := *out.AdminRecords[i].Resolution
			out.AdminRecords[i].Resolution = &resolution
		}
	}
	return out
}
