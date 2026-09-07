// Package sandboxpolicy validates and composes authored host policies without
// filesystem access, environment discovery, or launch effects.
package sandboxpolicy

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

var memoryLimitRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?|\.[0-9]+)([a-zA-Z]*)$`)

// ParseMemoryLimitBytes accepts Kubernetes-like decimal and binary quantities.
// K/KB, M/MB, G/GB and T/TB are powers of 1000, while Ki/KiB through Ti/TiB
// are powers of 1024. B is accepted explicitly; suffixes are case-insensitive.
func ParseMemoryLimitBytes(input string) (uint64, error) {
	return parseByteQuantity("memory limit", input)
}

// parseByteQuantity is the shared quantity grammar. The label names the axis
// being parsed so a refusal points at the field the operator actually wrote
// rather than at whichever axis happened to define the grammar first.
func parseByteQuantity(label, input string) (uint64, error) {
	value := strings.TrimSpace(input)
	if len(value) > 128 {
		return 0, fmt.Errorf("%s is too long", label)
	}
	match := memoryLimitRE.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("%s %q is invalid (examples: 512MiB, 4GB, 1.5GiB)", label, input)
	}
	factors := map[string]uint64{
		"b": 1,
		"k": 1_000, "kb": 1_000,
		"m": 1_000_000, "mb": 1_000_000,
		"g": 1_000_000_000, "gb": 1_000_000_000,
		"t": 1_000_000_000_000, "tb": 1_000_000_000_000,
		"ki": 1 << 10, "kib": 1 << 10,
		"mi": 1 << 20, "mib": 1 << 20,
		"gi": 1 << 30, "gib": 1 << 30,
		"ti": 1 << 40, "tib": 1 << 40,
	}
	factor, ok := factors[strings.ToLower(match[2])]
	if !ok {
		return 0, fmt.Errorf("%s %q has an unsupported suffix", label, input)
	}
	quantity, ok := new(big.Rat).SetString(match[1])
	if !ok || quantity.Sign() <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", label)
	}
	quantity.Mul(quantity, new(big.Rat).SetInt(new(big.Int).SetUint64(factor)))
	// Round fractional bytes up so the rendered hard ceiling is never smaller
	// than the quantity the operator authored.
	numerator := quantity.Num()
	denominator := quantity.Denom()
	bytes := new(big.Int).Quo(new(big.Int).Add(numerator, new(big.Int).Sub(denominator, big.NewInt(1))), denominator)
	if !bytes.IsUint64() || bytes.Sign() <= 0 {
		return 0, fmt.Errorf("%s %q is outside the supported byte range", label, input)
	}
	return bytes.Uint64(), nil
}

// CPUQuotaMicros derives the Linux 100ms-period ceiling from the exact authored
// decimal. A floating-point intermediate would round large or fractional limits.
func CPUQuotaMicros(input string) (uint64, error) {
	value := strings.TrimSpace(input)
	if len(value) > 128 || !cpuQuantityRE.MatchString(value) {
		return 0, fmt.Errorf("CPU limit must be a positive decimal number of cores")
	}
	cores, ok := new(big.Rat).SetString(value)
	if !ok || cores.Cmp(big.NewRat(1, 100)) < 0 {
		return 0, fmt.Errorf("CPU limit must be at least 0.01 cores")
	}
	cores.Mul(cores, big.NewRat(100000, 1))
	quota := new(big.Int).Quo(new(big.Int).Add(cores.Num(), new(big.Int).Sub(cores.Denom(), big.NewInt(1))), cores.Denom())
	if !quota.IsUint64() {
		return 0, fmt.Errorf("CPU limit is outside the supported range")
	}
	return quota.Uint64(), nil
}

var cpuQuantityRE = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]+)?|\.[0-9]+)$`)
