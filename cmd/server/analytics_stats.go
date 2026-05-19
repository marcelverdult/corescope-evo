// analytics_stats.go — shared stat/histogram helpers used by both the
// in-memory and SQL analytics implementations. Logic is identical so the two
// paths produce byte-equal histograms.

package main

import "math"

func rfStddevF64(arr []float64, avg float64) float64 {
	if len(arr) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range arr {
		d := v - avg
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(arr)))
}

func rfBuildHistogramF64(values []float64, bins int) map[string]interface{} {
	if len(values) == 0 {
		return map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0}
	}
	mn, mx := values[0], values[0]
	for _, v := range values {
		if v < mn {
			mn = v
		}
		if v > mx {
			mx = v
		}
	}
	rng := mx - mn
	if rng == 0 {
		rng = 1
	}
	binWidth := rng / float64(bins)
	counts := make([]int, bins)
	for _, v := range values {
		idx := int((v - mn) / binWidth)
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	binArr := make([]map[string]interface{}, bins)
	for i, c := range counts {
		binArr[i] = map[string]interface{}{"x": mn + float64(i)*binWidth, "w": binWidth, "count": c}
	}
	return map[string]interface{}{"bins": binArr, "min": mn, "max": mx}
}

func rfBuildHistogramInt(values []int, bins int) map[string]interface{} {
	if len(values) == 0 {
		return map[string]interface{}{"bins": []interface{}{}, "min": 0, "max": 0}
	}
	mnI, mxI := values[0], values[0]
	for _, v := range values {
		if v < mnI {
			mnI = v
		}
		if v > mxI {
			mxI = v
		}
	}
	mn, mx := float64(mnI), float64(mxI)
	rng := mx - mn
	if rng == 0 {
		rng = 1
	}
	binWidth := rng / float64(bins)
	counts := make([]int, bins)
	for _, v := range values {
		idx := int((float64(v) - mn) / binWidth)
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	binArr := make([]map[string]interface{}, bins)
	for i, c := range counts {
		binArr[i] = map[string]interface{}{"x": mn + float64(i)*binWidth, "w": binWidth, "count": c}
	}
	return map[string]interface{}{"bins": binArr, "min": mn, "max": mx}
}
