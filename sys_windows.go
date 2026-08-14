//go:build windows

package main

// diskUsageGB is a stub on Windows — the server runs on Linux;
// this keeps the main package testable on dev machines.
func diskUsageGB() (usedGB, totalGB float64) {
	return 0, 0
}
