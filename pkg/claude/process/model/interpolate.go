package model

// InterpolatePerformer resolves the documented runtime parameter surface.
// Configuration fields such as profile, timeout, retry, and wait values stay
// literal; validation rejects references there as inert.
func InterpolatePerformer(performer Performer, params map[string]string) Performer {
	performer.Prompt = interpolateParams(performer.Prompt, params)
	performer.Ask = interpolateParams(performer.Ask, params)
	performer.Run = interpolateParams(performer.Run, params)
	if performer.Args != nil {
		args := make([]string, len(performer.Args))
		for i, arg := range performer.Args {
			args[i] = interpolateParams(arg, params)
		}
		performer.Args = args
	}
	return performer
}

func interpolateParams(value string, params map[string]string) string {
	return paramRefPattern.ReplaceAllStringFunc(value, func(reference string) string {
		match := paramRefPattern.FindStringSubmatch(reference)
		if len(match) != 2 {
			return reference
		}
		return params[match[1]]
	})
}
