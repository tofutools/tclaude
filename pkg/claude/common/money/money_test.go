package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUSD(t *testing.T) {
	assert.Equal(t, "$0.42", USD(0.42))
	assert.Equal(t, "$999.99", USD(999.994))
	assert.Equal(t, "$1,000.00", USD(1000))
	assert.Equal(t, "$26,222.38", USD(26222.375))
	assert.Equal(t, "$1,234,567.89", USD(1234567.891))
}

// Real spend that would round to $0.00 must not read as free.
func TestUSDSubCent(t *testing.T) {
	assert.Equal(t, "<1¢", USD(0.004))
	assert.Equal(t, "$0.00", USD(0))
	assert.Equal(t, "$0.01", USD(0.005))
}
