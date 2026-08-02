package calculator

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// Params contains the operands and operation accepted by the calc command.
type Params struct {
	Operation string  `pos:"true" help:"Operation: add, sub, mul, or div"`
	Left      float64 `pos:"true" help:"Left operand"`
	Right     float64 `pos:"true" help:"Right operand"`
}

// Cmd returns the calculator command.
func Cmd() *cobra.Command {
	cmd := boa.CmdT[Params]{
		Use:         "calc",
		Aliases:     []string{"calculator"},
		Short:       "Perform basic arithmetic",
		Long:        "Perform addition, subtraction, multiplication, or division on two numbers.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFuncE: func(params *Params, cmd *cobra.Command, args []string) error {
			result, err := Calculate(params.Operation, params.Left, params.Right)
			if err != nil {
				return &boa.UserInputError{Err: err}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strconv.FormatFloat(result, 'f', -1, 64))
			return err
		},
	}.ToCobra()
	cmd.Example = "  tclaude calc add 2 3\n  tclaude calc div 10 4"
	return cmd
}

// Calculate applies a supported arithmetic operation to two numbers.
func Calculate(operation string, left, right float64) (float64, error) {
	switch strings.ToLower(operation) {
	case "add", "+":
		return left + right, nil
	case "sub", "subtract", "-":
		return left - right, nil
	case "mul", "multiply", "*", "x":
		return left * right, nil
	case "div", "divide", "/":
		if right == 0 {
			return 0, fmt.Errorf("cannot divide by zero")
		}
		return left / right, nil
	default:
		return 0, fmt.Errorf("unknown operation %q (use add, sub, mul, or div)", operation)
	}
}
