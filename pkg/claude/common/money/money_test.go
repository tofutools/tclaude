package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUSD(t *testing.T) {
	assert.Equal(t, "$0.42", USD(0.42))
	assert.Equal(t, "$99.99", USD(99.994))
	assert.Equal(t, "$1,000", USD(1000))
	assert.Equal(t, "$26,222", USD(26222.375))
	assert.Equal(t, "$1,234,568", USD(1234567.891))
}

// From $100 up the cents stop being worth their two digits.
func TestUSDDropsCentsOnLargeAmounts(t *testing.T) {
	assert.Equal(t, "$99.99", USD(99.99))
	assert.Equal(t, "$100", USD(100))
	assert.Equal(t, "$846", USD(845.88))
}

// Real spend that would round to $0.00 must not read as free.
func TestUSDSubCent(t *testing.T) {
	assert.Equal(t, "<1¢", USD(0.004))
	assert.Equal(t, "$0.00", USD(0))
	assert.Equal(t, "$0.01", USD(0.005))
}
