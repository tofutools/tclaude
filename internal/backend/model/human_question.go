package model

// Question preserves a separately authored question and its longer context.
// Prompt-only persisted performers keep their original presentation unchanged.
func (h HumanPerformer) Question() string {
	if h.Ask == "" {
		return h.Prompt
	}
	if h.Prompt == "" {
		return h.Ask
	}
	return h.Ask + "\n\n" + h.Prompt
}
