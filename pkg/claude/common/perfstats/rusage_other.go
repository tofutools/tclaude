//go:build !unix

package perfstats

func readCPUTimes() CPUTimes { return CPUTimes{} }
