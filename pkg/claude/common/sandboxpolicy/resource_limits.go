package sandboxpolicy

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strings"
)

// ResourceLimits is an independently optional Linux workload budget. Memory
// retains the operator's authored spelling for portable export and UI editing;
// ParseMemoryLimitBytes is the authority used by the Linux cgroup renderer.
type ResourceLimits struct {
	Memory      string   `json:"memory,omitempty"`
	MemoryBytes uint64   `json:"memory_bytes,omitempty"`
	CPU         *float64 `json:"cpu,omitempty"`
}

func (r ResourceLimits) Enabled() bool { return r.Memory != "" || r.CPU != nil }

var memoryLimitRE = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?|\.[0-9]+)([a-zA-Z]*)$`)

// ParseMemoryLimitBytes accepts Kubernetes-like decimal and binary quantities.
// K/KB, M/MB, G/GB and T/TB are powers of 1000, while Ki/KiB through Ti/TiB
// are powers of 1024. B is accepted explicitly; suffixes are case-insensitive.
func ParseMemoryLimitBytes(input string) (uint64, error) {
	value := strings.TrimSpace(input)
	match := memoryLimitRE.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("memory limit %q is invalid (examples: 512MiB, 4GB, 1.5GiB)", input)
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
		return 0, fmt.Errorf("memory limit %q has an unsupported suffix", input)
	}
	quantity, ok := new(big.Rat).SetString(match[1])
	if !ok || quantity.Sign() <= 0 {
		return 0, fmt.Errorf("memory limit must be greater than zero")
	}
	quantity.Mul(quantity, new(big.Rat).SetInt(new(big.Int).SetUint64(factor)))
	// Round fractional bytes up so the rendered hard ceiling is never smaller
	// than the quantity the operator authored.
	numerator := quantity.Num()
	denominator := quantity.Denom()
	bytes := new(big.Int).Quo(new(big.Int).Add(numerator, new(big.Int).Sub(denominator, big.NewInt(1))), denominator)
	if !bytes.IsUint64() || bytes.Sign() <= 0 {
		return 0, fmt.Errorf("memory limit %q is outside the supported byte range", input)
	}
	return bytes.Uint64(), nil
}

func NormalizeResourceLimits(in ResourceLimits) (ResourceLimits, error) {
	out := ResourceLimits{Memory: strings.TrimSpace(in.Memory)}
	if out.Memory != "" {
		bytes, err := ParseMemoryLimitBytes(out.Memory)
		if err != nil {
			return ResourceLimits{}, err
		}
		out.MemoryBytes = bytes
	} else if in.MemoryBytes != 0 {
		return ResourceLimits{}, fmt.Errorf("memory_bytes is derived and requires an authored memory limit")
	}
	if in.CPU != nil {
		if math.IsNaN(*in.CPU) || math.IsInf(*in.CPU, 0) || *in.CPU <= 0 {
			return ResourceLimits{}, fmt.Errorf("CPU limit must be a positive finite number of cores")
		}
		if *in.CPU < float64(CPUCgroupMinimumQuotaMicros)/float64(CPUCgroupPeriodMicros) {
			return ResourceLimits{}, fmt.Errorf("CPU limit must be at least 0.01 cores for Linux's minimum cgroup quota")
		}
		value := *in.CPU
		out.CPU = &value
	}
	return out, nil
}

// ResourceCgroupRequested reports whether a launch has to create the per-agent
// cgroup. An authored ceiling requires one, and so does `resource-only` with no
// ceiling at all: that spelling exists for the cgroup itself, so a limitless
// resource-only launch is an accounting boundary — per-agent counters, OOM
// attribution through memory.events, and a kill handle for the whole workload
// tree — rather than the silent no-op it would otherwise be.
func ResourceCgroupRequested(limits ResourceLimits, implementation Implementation) bool {
	return limits.Enabled() || implementation == ImplementationResourceOnly
}

// ValidateResourceLimitTarget checks the MVP's product compatibility boundary.
// Host controller/delegation probes are deliberately separate and run only at
// the Linux launch seam.
//
// Only `off` is refused. Limits are orthogonal to the confinement layer — the
// cgroup is created and joined by the pane wrapper, not by bubblewrap — so
// every other implementation may carry them, including `resource-only`, which
// exists precisely to pair a cgroup with no access boundary at all. `off`
// stays refused so that the spelling meaning "tclaude enforces nothing here"
// keeps meaning exactly that.
func ValidateResourceLimitTarget(limits ResourceLimits, implementation Implementation, platform string) error {
	if !ResourceCgroupRequested(limits, implementation) {
		return nil
	}
	if platform != "linux" {
		if !limits.Enabled() {
			return fmt.Errorf("sandbox implementation %s needs Linux cgroup v2 for its per-agent cgroup; %s launches cannot create one",
				ImplementationResourceOnly, platform)
		}
		return fmt.Errorf("resource limits are Linux only; %s launches cannot enforce this profile", platform)
	}
	if implementation == ImplementationOff {
		return fmt.Errorf("sandbox implementation %s cannot carry resource limits; use %s to enforce them with no access confinement, or %s to keep the harness's own sandbox as well",
			ImplementationOff, ImplementationResourceOnly, ImplementationHarnessBuiltin)
	}
	return nil
}

const CPUCgroupPeriodMicros uint64 = 100_000
const CPUCgroupMinimumQuotaMicros uint64 = 1_000

// CPUQuotaMicros converts cores to cpu.max quota with deterministic upward
// rounding. A 100ms period gives ordinary decimal core values exact results.
func CPUQuotaMicros(cores float64) (uint64, error) {
	if math.IsNaN(cores) || math.IsInf(cores, 0) || cores <= 0 {
		return 0, fmt.Errorf("CPU limit must be a positive finite number of cores")
	}
	quota := math.Ceil(cores * float64(CPUCgroupPeriodMicros))
	if quota < float64(CPUCgroupMinimumQuotaMicros) {
		return 0, fmt.Errorf("CPU limit must be at least 0.01 cores for Linux's minimum cgroup quota")
	}
	if quota >= math.Exp2(64) {
		return 0, fmt.Errorf("CPU limit is outside the supported range")
	}
	return uint64(quota), nil
}

func cloneResourceLimits(in ResourceLimits) ResourceLimits {
	out := ResourceLimits{Memory: in.Memory, MemoryBytes: in.MemoryBytes}
	if in.CPU != nil {
		value := *in.CPU
		out.CPU = &value
	}
	return out
}

// requireResourceLimitsContained prevents a relaunch/child snapshot from
// weakening an inherited hard ceiling. Adding a new ceiling is a restriction.
func requireResourceLimitsContained(parent, child ResourceLimits) error {
	if parent.Memory != "" {
		parentBytes, err := ParseMemoryLimitBytes(parent.Memory)
		if err != nil {
			return fmt.Errorf("parent memory limit: %w", err)
		}
		if child.Memory == "" {
			return fmt.Errorf("memory resource limit from the parent snapshot is not preserved")
		}
		childBytes, err := ParseMemoryLimitBytes(child.Memory)
		if err != nil {
			return fmt.Errorf("child memory limit: %w", err)
		}
		if childBytes > parentBytes {
			return fmt.Errorf("child memory resource limit is weaker than the parent snapshot")
		}
	}
	if parent.CPU != nil {
		if child.CPU == nil {
			return fmt.Errorf("CPU resource limit from the parent snapshot is not preserved")
		}
		if *child.CPU > *parent.CPU {
			return fmt.Errorf("child CPU resource limit is weaker than the parent snapshot")
		}
	}
	return nil
}
