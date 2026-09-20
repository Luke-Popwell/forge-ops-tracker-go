package forgeops

import (
	"reflect"
	"strconv"
	"testing"
)

func TestBucketForReturnsTheSmallestBoundaryADurationFitsUnder(t *testing.T) {
	cases := map[float64]string{10: "50", 50: "50", 50.5: "100", 4999: "5000"}
	for duration, want := range cases {
		if got := bucketFor(duration); got != want {
			t.Errorf("bucketFor(%v) = %q, want %q", duration, got, want)
		}
	}
}

func TestBucketForReturnsInfForAnythingLargerThanTheLargestBoundary(t *testing.T) {
	for _, duration := range []float64{10001, 1000000} {
		if got := bucketFor(duration); got != "inf" {
			t.Errorf("bucketFor(%v) = %q, want inf", duration, got)
		}
	}
}

func TestBucketForPutsADurationExactlyOnABoundaryIntoThatBoundarysOwnBucket(t *testing.T) {
	for _, boundary := range histogramBoundariesMs {
		if got := bucketFor(float64(boundary)); got != strconv.Itoa(boundary) {
			t.Errorf("bucketFor(%d) = %q, want %d", boundary, got, boundary)
		}
	}
}

func TestHistogramBoundariesMatchTheServersHistogramPercentile(t *testing.T) {
	// app/services/histogram_percentile.rb and every other SDK must agree on this exact list.
	want := []int{50, 100, 250, 500, 1000, 2500, 5000, 10000}
	if !reflect.DeepEqual(histogramBoundariesMs, want) {
		t.Errorf("histogramBoundariesMs = %v, want %v", histogramBoundariesMs, want)
	}
}
