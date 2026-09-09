package model

import "fmt"

type ToolGovernance string

const (
	ToolGovernanceAllow ToolGovernance = "allow"
	ToolGovernanceAsk   ToolGovernance = "ask"
	ToolGovernanceDeny  ToolGovernance = "deny"
)

func (mode ToolGovernance) Validate() error {
	switch mode {
	case "", ToolGovernanceAllow, ToolGovernanceAsk, ToolGovernanceDeny:
		return nil
	}
	return fmt.Errorf("unsupported tool governance %q", mode)
}
