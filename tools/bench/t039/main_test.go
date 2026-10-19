package main

import (
	"testing"
	"time"
)

func TestPercentileIndexUsesNearestRank(t *testing.T) {
	for _, test := range []struct {
		count      int
		percentile float64
		want       int
	}{
		{count: 1, percentile: 0.95, want: 0},
		{count: 30, percentile: 0.50, want: 14},
		{count: 30, percentile: 0.95, want: 28},
		{count: 100, percentile: 0.95, want: 94},
	} {
		if got := percentileIndex(test.count, test.percentile); got != test.want {
			t.Fatalf("percentileIndex(%d, %.2f)=%d, want %d", test.count, test.percentile, got, test.want)
		}
	}
}

func TestValidGitRevisionAcceptsFullCommitHashesOnly(t *testing.T) {
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: "0123456789abcdef0123456789abcdef01234567", want: true},
		{value: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", want: true},
		{value: "0123456789abcdef", want: false},
		{value: "0123456789abcdef0123456789abcdef0123456G", want: false},
		{value: "0123456789ABCDEF0123456789ABCDEF01234567", want: false},
	} {
		if got := validGitRevision(test.value); got != test.want {
			t.Fatalf("validGitRevision(%q)=%t, want %t", test.value, got, test.want)
		}
	}
}

func TestBenchmarkClockMeasuresPositiveElapsedTime(t *testing.T) {
	started := benchmarkClockNow()
	time.Sleep(time.Millisecond)
	if got := benchmarkClockElapsedNS(started); got <= 0 {
		t.Fatalf("benchmark clock elapsed=%d ns, want positive duration", got)
	}
}
