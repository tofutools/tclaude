package agentd

import "regexp"

var openCodeAgentIDRE = regexp.MustCompile(`^agt_[0-9a-f]+$`)
