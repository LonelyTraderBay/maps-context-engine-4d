//go:build !windows

package main

import "time"

var benchmarkClockOrigin = time.Now()

func benchmarkClockNow() int64 { return time.Since(benchmarkClockOrigin).Nanoseconds() }

func benchmarkClockElapsedNS(start int64) int64 { return benchmarkClockNow() - start }

func benchmarkClockSource() string { return "Go monotonic time.Since" }
