//go:build unix

package perfstats

import "syscall"

func readCPUTimes() CPUTimes {
	var self, children syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &self) != nil ||
		syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children) != nil {
		return CPUTimes{}
	}
	return CPUTimes{
		SelfUserSeconds:     tvSeconds(self.Utime),
		SelfSystemSeconds:   tvSeconds(self.Stime),
		ChildrenUserSeconds: tvSeconds(children.Utime),
		ChildrenSysSeconds:  tvSeconds(children.Stime),
		Available:           true,
	}
}

func tvSeconds(tv syscall.Timeval) float64 {
	return float64(tv.Sec) + float64(tv.Usec)/1e6
}
