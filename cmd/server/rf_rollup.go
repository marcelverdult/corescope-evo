// rf_rollup.go — RF analytics rollup: schema, bin packing, single-hour recompute.
// See .specs/2026-05-19-rf-analytics-rollup-design.md.

package main

import (
	"encoding/binary"
	"fmt"
)

// Fixed histogram bins. Values outside the range clamp to the end bin.
const (
	rfSnrBinMin, rfSnrBinWidth, rfSnrBinCount    = -30, 1, 50
	rfRssiBinMin, rfRssiBinWidth, rfRssiBinCount = -130, 1, 110
	rfSizeBinMin, rfSizeBinWidth, rfSizeBinCount = 0, 4, 64
)

// rfBinIndex maps a value to a clamped [0,count) bin index.
func rfBinIndex(v, min, width, count int) int {
	i := (v - min) / width
	if i < 0 {
		return 0
	}
	if i >= count {
		return count - 1
	}
	return i
}

// rfPackBins encodes per-bin counts as little-endian int16. Counts above the
// int16 max are clamped (a single hour/observer cell never approaches 32767).
func rfPackBins(counts []int) []byte {
	b := make([]byte, len(counts)*2)
	for i, c := range counts {
		if c > 32767 {
			c = 32767
		}
		if c < 0 {
			c = 0
		}
		binary.LittleEndian.PutUint16(b[i*2:], uint16(c))
	}
	return b
}

// rfUnpackBins decodes a packed blob into count integers. A nil/short blob
// yields a zero slice of length count.
func rfUnpackBins(b []byte, count int) []int {
	out := make([]int, count)
	for i := 0; i < count && (i*2+1) < len(b); i++ {
		out[i] = int(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

var _ = fmt.Sprintf // kept for later tasks in this file
