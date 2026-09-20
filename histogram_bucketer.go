package forgeops

import "strconv"

// histogramBoundariesMs are the fixed latency-range upper bounds PerformanceFlusher buckets every
// recorded duration into, the building block behind an approximate distribution (not just
// count/sum/max) alongside every transaction bucket it already tallies. The server merges these
// counts across matching samples at read time and walks cumulative counts to approximate a
// percentile, accurate to the bucket width: this SDK never stores the raw duration list a true
// percentile would need. Ported from gems/forge_ops_tracker/lib/forge_ops_tracker/histogram_bucketer.rb.
//
// Duplicated on the server side, in app/services/histogram_percentile.rb. Change one, change the
// other, or a released SDK version and the server it talks to would silently disagree about what
// each bucket label means.
var histogramBoundariesMs = []int{50, 100, 250, 500, 1000, 2500, 5000, 10000}

// bucketFor returns the label of the smallest boundary durationMs fits under, or "inf" for
// anything larger than the largest boundary. A string, not an int: this travels as a JSON object
// key once flushed, and JSON object keys are always strings.
func bucketFor(durationMs float64) string {
	for _, boundary := range histogramBoundariesMs {
		if durationMs <= float64(boundary) {
			return strconv.Itoa(boundary)
		}
	}
	return "inf"
}
