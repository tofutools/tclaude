//go:build !linux && !darwin

package hostmetrics

import "fmt"

func readLoad() ([3]float64, error)           { return [3]float64{}, fmt.Errorf("unsupported platform") }
func readMemory() (*Memory, error)            { return nil, fmt.Errorf("unsupported platform") }
func readDisk(string) (uint64, uint64, error) { return 0, 0, fmt.Errorf("unsupported platform") }
