//go:build linux

package main

import "syscall"

// diskUsageGB returns used and total disk space for / in GB.
func diskUsageGB() (usedGB, totalGB float64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return 0, 0
	}
	total := float64(stat.Blocks*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
	free := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
	return total - free, total
}
