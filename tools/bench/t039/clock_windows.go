//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	queryPerformanceCounter   = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceCounter")
	queryPerformanceFrequency = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryPerformanceFrequency")
	performanceFrequency      int64
)

func init() {
	var frequency int64
	result, _, callErr := queryPerformanceFrequency.Call(uintptr(unsafe.Pointer(&frequency)))
	if result == 0 || frequency <= 0 {
		panic(fmt.Sprintf("QueryPerformanceFrequency failed: %v", callErr))
	}
	performanceFrequency = frequency
}

func benchmarkClockNow() int64 {
	var counter int64
	result, _, callErr := queryPerformanceCounter.Call(uintptr(unsafe.Pointer(&counter)))
	if result == 0 {
		panic(fmt.Sprintf("QueryPerformanceCounter failed: %v", callErr))
	}
	return counter
}

func benchmarkClockElapsedNS(start int64) int64 {
	delta := benchmarkClockNow() - start
	return (delta/performanceFrequency)*1_000_000_000 + (delta%performanceFrequency)*1_000_000_000/performanceFrequency
}

func benchmarkClockSource() string { return "Windows QueryPerformanceCounter" }
