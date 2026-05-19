package main

import "testing"

func TestRFBinIndexAndPacking(t *testing.T) {
	if got := rfBinIndex(-30, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != 0 {
		t.Fatalf("snr -30 -> bin %d, want 0", got)
	}
	if got := rfBinIndex(1000, rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount); got != rfSnrBinCount-1 {
		t.Fatalf("snr 1000 clamps to %d, want %d", got, rfSnrBinCount-1)
	}
	counts := make([]int, rfSnrBinCount)
	counts[0] = 5
	counts[49] = 7
	packed := rfPackBins(counts)
	if len(packed) != rfSnrBinCount*2 {
		t.Fatalf("packed len %d, want %d", len(packed), rfSnrBinCount*2)
	}
	out := rfUnpackBins(packed, rfSnrBinCount)
	if out[0] != 5 || out[49] != 7 {
		t.Fatalf("unpack mismatch: %v", out[:1])
	}
}
