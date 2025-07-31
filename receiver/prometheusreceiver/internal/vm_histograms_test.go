package internal

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"go.opentelemetry.io/collector/pdata/pmetric"
)

func TestVMHistogramParseRange(t *testing.T) {
	tests := []struct {
		vmrange   string
		wantStart float64
		wantEnd   float64
		wantErr   bool
	}{
		{
			vmrange:   "4.084e+02...4.642e+02",
			wantStart: 4.084e+02,
			wantEnd:   4.642e+02,
		},
		{
			vmrange:   "0...0.000e+00",
			wantStart: 0,
			wantEnd:   0,
		},
		{
			vmrange:   "0...0",
			wantStart: 0,
			wantEnd:   0,
		},
		{
			vmrange:   "0...1.000e-09",
			wantStart: 0,
			wantEnd:   1e-9,
		},
		{
			vmrange:   "-Inf...0",
			wantStart: math.Inf(-1),
			wantEnd:   0,
		},
		{
			vmrange:   "1.000e+18...+Inf",
			wantStart: 1e18,
			wantEnd:   math.Inf(1),
		},
		{
			vmrange: "",
			wantErr: true,
		},
		{
			vmrange: "1...",
			wantErr: true,
		},
		{
			vmrange: "1.0e...10",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.vmrange, func(t *testing.T) {
			gotStart, gotEnd, err := vmHistogramParseRange(tt.vmrange)

			if tt.wantErr {
				if err == nil {
					t.Errorf("vmHistogramParseRange() expected error but got none")
					return
				}
				if !errors.Is(err, errInvalidVMHistogramRange) {
					t.Errorf("vmHistogramParseRange() error = %v, expected %v", err, errInvalidVMHistogramRange)
					return
				}
			} else {
				if err != nil {
					t.Errorf("vmHistogramParseRange() unexpected error = %v", err)
					return
				}
			}

			if gotStart != tt.wantStart {
				t.Errorf("vmHistogramParseRange() start = %v, want %v", gotStart, tt.wantStart)
			}
			if gotEnd != tt.wantEnd {
				t.Errorf("vmHistogramParseRange() end = %v, want %v", gotEnd, tt.wantEnd)
			}
		})
	}
}

func TestVMHistogramGetScale(t *testing.T) {
	tests := []struct {
		vmrange   string
		wantScale int32
	}{
		// Examples of conventional VM Histogram base-10 boundaries.
		// These map to the closest scale but they're not exact.
		{vmrange: "4.642e+01...5.275e+01", wantScale: 2},
		{vmrange: "4.084e+02...4.642e+02", wantScale: 2},
		// Exact matches to base-2 boundaries.
		{vmrange: "1...4", wantScale: -1},
		{vmrange: "1...2", wantScale: 0},
		{vmrange: "1...1.41421", wantScale: 1},
		{vmrange: "1...1.18921", wantScale: 2},
		{vmrange: "1...1.09051", wantScale: 3},
	}

	for _, tt := range tests {
		t.Run(tt.vmrange, func(t *testing.T) {
			start, end, err := vmHistogramParseRange(tt.vmrange)
			if err != nil {
				t.Errorf("vmHistogramParseRange() unexpected error = %v", err)
			}
			gotScale := vmHistogramGetScale(start, end)
			if gotScale != tt.wantScale {
				t.Errorf("vmHistogramGetScale() = %v, want %v", gotScale, tt.wantScale)
			}
		})
	}
}

func TestVMConvertBuckets(t *testing.T) {
	tests := []struct {
		name          string
		source        []*dataPoint
		scale         int32
		wantZeroCount uint64
		wantOffset    int32
		wantCounts    []uint64
	}{
		{
			name: "basic test case",
			source: []*dataPoint{
				{boundary: 0, value: 5},  // zero count
				{boundary: 2, value: 10}, // index = 0
				{boundary: 4, value: 15}, // index = 1
			},
			scale: 0, // base = 2

			wantZeroCount: 5,
			wantOffset:    0,
			wantCounts:    []uint64{10, 15},
		},
		{
			name: "scale 3 with offset and skipped bucket",
			source: []*dataPoint{
				{boundary: 0, value: 5},        // zero count
				{boundary: 1.54221, value: 10}, // index = 4
				{boundary: 1.83401, value: 15}, // index = 6
			},
			scale: 3, // base = 1.09051

			wantZeroCount: 5,
			wantOffset:    4,
			wantCounts:    []uint64{10, 0, 15},
		},
		{
			name: "VM base-10 boundaries forced into scale 2",
			source: []*dataPoint{
				{boundary: 0, value: 5},          // zero count
				{boundary: 4.642e+02, value: 10}, // closest index = 34 (actual boundary = 430.6)
				{boundary: 5.995e+02, value: 15}, // closest index = 36 (actual boundary = 608.9)
				{boundary: 1.000e+03, value: 20}, // closest index = 39 (actual boundary = 1024)
			},
			scale:         2, // base = 1.18921 (vs. source VM base of 1.136)
			wantZeroCount: 5,
			wantOffset:    34,
			wantCounts:    []uint64{10, 0, 15, 0, 0, 20},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := pmetric.NewExponentialHistogramDataPoint()
			dest.SetScale(tt.scale)

			vmConvertBuckets(dest, tt.source)

			if dest.ZeroCount() != tt.wantZeroCount {
				t.Errorf("vmConvertBuckets() zero count = %v, want %v", dest.ZeroCount(), tt.wantZeroCount)
			}
			if dest.Positive().Offset() != tt.wantOffset {
				t.Errorf("vmConvertBuckets() offset = %v, want %v", dest.Positive().Offset(), tt.wantOffset)
			}
			if !reflect.DeepEqual(dest.Positive().BucketCounts().AsRaw(), tt.wantCounts) {
				t.Errorf("vmConvertBuckets() bucket counts = %v, want %v", dest.Positive().BucketCounts().AsRaw(), tt.wantCounts)
			}
		})
	}
}

func TestVMEstimateSum(t *testing.T) {
	tests := []struct {
		name     string
		source   []*dataPoint
		expected float64
	}{
		{
			name: "basic",
			source: []*dataPoint{
				{prevBoundary: 0, boundary: 1, value: 5},
				{prevBoundary: 1, boundary: 2, value: 10},
				{prevBoundary: 2, boundary: 4, value: 15},
			},
			expected: 62.5, // 0.5*5 + 1.5*10 + 3*15
		},
		{
			name: "with zero bucket",
			source: []*dataPoint{
				{prevBoundary: 0, boundary: 0, value: 5},
				{prevBoundary: 0, boundary: 1, value: 10},
				{prevBoundary: 1, boundary: 2, value: 15},
			},
			expected: 27.5, // 0*5 + 0.5*10 + 1.5*15
		},
		{
			name: "with infinity bucket",
			source: []*dataPoint{
				{prevBoundary: 0, boundary: 1, value: 5},
				{prevBoundary: 1, boundary: 2, value: 10},
				{prevBoundary: 2, boundary: 4, value: 15},
				{prevBoundary: 4, boundary: math.Inf(1), value: 20},
			},
			expected: 62.5, // 0.5*5 + 1.5*10 + 3*15
		},
		{
			name: "sparse buckets",
			source: []*dataPoint{
				{prevBoundary: 1, boundary: 2, value: 5},
				{prevBoundary: 4, boundary: 8, value: 10},
			},
			expected: 67.5, // 1.5*5 + 6*10
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := vmEstimateSum(tt.source)
			if got != tt.expected {
				t.Errorf("vmEstimateSum() = %v, want %v", got, tt.expected)
			}
		})
	}
}
