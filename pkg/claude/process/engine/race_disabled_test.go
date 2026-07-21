//go:build !race

package engine

// raceDetectorEnabled reports at build time whether this test binary carries
// race instrumentation. Only the wall/performance-sensitive short-TTL lease
// drive consults it; every other regression runs under -race unchanged.
const raceDetectorEnabled = false
