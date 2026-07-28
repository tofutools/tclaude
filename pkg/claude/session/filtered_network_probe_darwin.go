//go:build darwin

package session

func probeFilteredNetworkPrerequisite() FilteredNetworkPrerequisite {
	return FilteredNetworkPrerequisite{
		Detail: "the approved filtered gateway is Linux-only; Seatbelt has no filtered-egress applier",
	}
}
