package harness

// Copilot's AIU contract, established by TCL-982:
// 1 AI credit = 10^9 nano-AIU = $0.01 of GROSS subscription value. This is
// not net billed spend; account allowances, pools and discounts are outside
// the model. Keep the conversion here so Copilot's native unit is converted
// consistently everywhere it enters tclaude's what-if cost surfaces.
const (
	copilotNanoAIUPerCredit     = 1_000_000_000
	copilotNanoAIUPerVirtualUSD = 100_000_000_000
	copilotGrossUSDPerCredit    = 0.01
)

// CopilotVirtualCost is the two display forms of one folded Copilot total.
// Credits remain the vendor-native value; USD is explicitly hypothetical
// gross subscription value, never actual billed spend. The folded int64 stays
// authoritative in CopilotUsageSnapshot; these float64 forms are derived only
// for persistence and display, with rounding deferred to the UI.
type CopilotVirtualCost struct {
	USD     float64
	Credits float64
}

// CopilotVirtualCostFromNanoAIU converts a folded session total. A nil,
// zero, or negative total means Copilot did not provide a positive measured
// cost and must remain absent from the virtual-cost display rather than
// rendering a measured-looking $0.00.
func CopilotVirtualCostFromNanoAIU(totalNanoAIU *int64) (CopilotVirtualCost, bool) {
	if totalNanoAIU == nil || *totalNanoAIU <= 0 {
		return CopilotVirtualCost{}, false
	}
	credits := float64(*totalNanoAIU) / copilotNanoAIUPerCredit
	return CopilotVirtualCost{
		USD:     float64(*totalNanoAIU) / copilotNanoAIUPerVirtualUSD,
		Credits: credits,
	}, true
}

// CopilotVirtualCreditsFromUSD converts a persisted virtual-dollar delta back
// to Copilot's native credits for a tooltip. The stored USD is the unrounded
// value produced by CopilotVirtualCostFromNanoAIU; UI formatting happens only
// after this conversion, so a row never loses credits to cents rounding.
func CopilotVirtualCreditsFromUSD(virtualUSD float64) float64 {
	if virtualUSD <= 0 {
		return 0
	}
	return virtualUSD / copilotGrossUSDPerCredit
}
