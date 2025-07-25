package internal

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

// This file contains logic for handling VictoriaMetrics Histograms.
// See: https://docs.victoriametrics.com/victoriametrics/keyconcepts/#histogram

const (
	vmHistogramRangeLabel = "vmrange"
)

var (
	errInvalidVMHistogramRange = errors.New("invalid 'vmrange' label value on histogram bucket")
)

func vmHistogramParseRange(vmrange string) (start, end float64, err error) {
	before, after, ok := strings.Cut(vmrange, "...")
	if !ok {
		return 0, 0, errInvalidVMHistogramRange
	}
	start, err = strconv.ParseFloat(before, 64)
	if err != nil {
		return 0, 0, errInvalidVMHistogramRange
	}
	end, err = strconv.ParseFloat(after, 64)
	if err != nil {
		return 0, 0, errInvalidVMHistogramRange
	}
	return start, end, nil
}

func vmHistogramGetScale(start, end float64) int32 {
	// Compute "scale" as per the OTLP spec:
	//   https://opentelemetry.io/docs/specs/otel/metrics/data-model/#exponential-scale
	//
	//  base = 2**(2**(-scale)) = end / start
	return int32(math.Round(-math.Log2(math.Log2(end / start))))
}

func vmConvertBuckets(dest pmetric.ExponentialHistogramDataPoint, source []*dataPoint) {
	// base is the multiplier from one bucket boundary to the next:
	//   base = 2**(2**(-scale))
	// To calculate a logarithm with a base of "base", we'll use the formula:
	//   log_base(x) = log2(x) / log2(base)
	// So what we actually want to know is log2(base) = 2**(-scale)
	log2base := math.Pow(2, float64(-dest.Scale()))

	offsetInitialized := false
	for _, dp := range source {
		if dp.boundary == 0 {
			dest.SetZeroCount(uint64(dp.value))
			continue
		}

		// Calculate the index of the bucket given:
		//   boundary = base**(index+1)
		// So:
		//   index = log_base(boundary) - 1 = log2(boundary) / log2(base) - 1
		index := int32(math.Round(math.Log2(dp.boundary)/log2base - 1))

		// Initialize the offset to the index of the first non-zero boundary.
		if !offsetInitialized {
			offsetInitialized = true
			dest.Positive().SetOffset(index)
		}

		// Buckets are dense, starting at the offset, so we may need to fill in zeros.
		position := int(index - dest.Positive().Offset())
		for dest.Positive().BucketCounts().Len() <= position {
			dest.Positive().BucketCounts().Append(0)
		}

		// Add to the existing value in case inexact (non-base-2) boundaries cause
		// us to merge multiple source buckets into one destination bucket.
		dest.Positive().BucketCounts().SetAt(position, dest.Positive().BucketCounts().At(position)+uint64(dp.value))
	}
}
