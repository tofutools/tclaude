package hostmetrics

import (
	"bufio"
	"fmt"
	"math"
	"strconv"
	"strings"
)

func parseLoad(raw string) ([3]float64, error) {
	var out [3]float64
	fields := strings.Fields(strings.Trim(raw, "{} \r\n\t"))
	if len(fields) < 3 {
		return out, fmt.Errorf("missing load averages")
	}
	for i := range out {
		n, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
			return out, fmt.Errorf("invalid load average")
		}
		out[i] = n
	}
	return out, nil
}
func parseLinuxMemory(raw string) (*Memory, error) {
	counters := map[string]uint64{}
	wanted := map[string]bool{"MemTotal": true, "MemAvailable": true, "MemFree": true, "Buffers": true, "Cached": true, "SReclaimable": true, "Shmem": true}
	scan := bufio.NewScanner(strings.NewReader(raw))
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) == 0 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		if !wanted[key] {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return nil, fmt.Errorf("invalid %s counter", key)
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, err
		}
		n, err = multiply(n, 1024)
		if err != nil {
			return nil, err
		}
		counters[key] = n
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	total := counters["MemTotal"]
	if total == 0 {
		return nil, fmt.Errorf("missing total memory")
	}
	available, ok := counters["MemAvailable"]
	if !ok {
		if _, freeOK := counters["MemFree"]; !freeOK {
			return nil, fmt.Errorf("missing available memory")
		}
		// Conservative fallback for kernels without MemAvailable. Sum safely,
		// capped at total; shared memory is not counted as reclaimable cache.
		for _, key := range []string{"MemFree", "Buffers", "Cached", "SReclaimable"} {
			available += min(counters[key], total-available)
		}
		available -= min(available, counters["Shmem"])
	}
	return &Memory{TotalBytes: total, AvailableBytes: min(total, available), AvailableEstimated: !ok}, nil
}

func parseMacMemory(raw string, total uint64) (*Memory, error) {
	if total == 0 {
		return nil, fmt.Errorf("missing total memory")
	}
	first, rest, ok := strings.Cut(raw, "\n")
	if !ok {
		return nil, fmt.Errorf("missing vm_stat header")
	}
	_, sizePart, ok := strings.Cut(first, "page size of ")
	if !ok {
		return nil, fmt.Errorf("missing vm_stat page size")
	}
	fields := strings.Fields(sizePart)
	if len(fields) == 0 {
		return nil, fmt.Errorf("missing vm_stat page size")
	}
	pageSize, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || pageSize == 0 {
		return nil, fmt.Errorf("invalid vm_stat page size")
	}
	wanted := map[string]bool{"Pages free": true, "Pages inactive": true, "Pages speculative": true}
	counters := map[string]uint64{}
	scan := bufio.NewScanner(strings.NewReader(rest))
	for scan.Scan() {
		key, value, found := strings.Cut(scan.Text(), ":")
		key = strings.TrimSpace(key)
		if !found || !wanted[key] {
			continue
		}
		n, e := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if e != nil {
			return nil, e
		}
		n, e = multiply(n, pageSize)
		if e != nil {
			return nil, e
		}
		counters[key] = n
	}
	if err = scan.Err(); err != nil {
		return nil, err
	}
	if _, ok = counters["Pages free"]; !ok {
		return nil, fmt.Errorf("missing free pages")
	}
	if _, ok = counters["Pages inactive"]; !ok {
		return nil, fmt.Errorf("missing inactive pages")
	}
	// vm_stat prints free excluding speculative; adding these three does not
	// double count. Purgeable pages overlap other lists and are not added.
	// This is an estimate, not macOS's memory-pressure metric.
	available := uint64(0)
	for _, key := range []string{"Pages free", "Pages inactive", "Pages speculative"} {
		available += min(counters[key], total-available)
	}
	return &Memory{TotalBytes: total, AvailableBytes: available, AvailableEstimated: true}, nil
}
