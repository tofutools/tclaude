package calculator

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalculate(t *testing.T) {
	tests := []struct {
		name      string
		operation string
		left      float64
		right     float64
		want      float64
	}{
		{name: "add", operation: "add", left: 2, right: 3, want: 5},
		{name: "subtract", operation: "-", left: 8, right: 3, want: 5},
		{name: "multiply", operation: "mul", left: 2.5, right: 4, want: 10},
		{name: "divide", operation: "/", left: 7, right: 2, want: 3.5},
		{name: "case insensitive", operation: "ADD", left: -2, right: 0.5, want: -1.5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Calculate(tt.operation, tt.left, tt.right)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCalculateErrors(t *testing.T) {
	_, err := Calculate("div", 1, 0)
	assert.EqualError(t, err, "cannot divide by zero")

	_, err = Calculate("power", 2, 3)
	assert.EqualError(t, err, `unknown operation "power" (use add, sub, mul, or div)`)
}

func TestCmdPrintsResult(t *testing.T) {
	cmd := Cmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"div", "10", "4"})

	require.NoError(t, cmd.Execute())
	assert.Equal(t, "2.5\n", output.String())
}

func TestCmdRejectsInvalidInput(t *testing.T) {
	cmd := Cmd()
	cmd.SetArgs([]string{"div", "10", "0"})

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot divide by zero")
}
