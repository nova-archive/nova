//go:build novasim

package model

import (
	"math"
	"sort"
)

// Concentration metrics quantify how unevenly replicas (or load, or
// capacity) are spread across nodes and failure domains. They are the
// signals Nova should ALERT on (federation.concentrated / .homogeneous),
// not invariants the placement engine enforces by refusing to place. See
// the design doc's "alert, not prevent" stance.

// Gini returns the Gini coefficient of a set of non-negative values, in
// [0, 1]. 0 is perfect equality (every value identical); the maximum,
// (n-1)/n, is reached when a single value holds the entire mass. An empty
// slice or all-zero slice returns 0 (no mass to concentrate).
//
// Definition: G = (Σ_i Σ_j |x_i - x_j|) / (2 n Σ_k x_k), computed in
// O(n log n) via the sorted-prefix identity rather than the O(n²) double sum.
func Gini(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	xs := make([]float64, n)
	copy(xs, values)
	sort.Float64s(xs)

	var sum float64
	for _, v := range xs {
		if v < 0 {
			v = 0
		}
		sum += v
	}
	if sum == 0 {
		return 0
	}
	// Σ_i Σ_j |x_i - x_j| = 2 * Σ_i ( (2i - n + 1) * x_i ) for sorted x (0-based i).
	var weighted float64
	for i, v := range xs {
		weighted += float64(2*i-n+1) * v
	}
	return weighted / (float64(n) * sum)
}

// ShannonEntropy returns the Shannon entropy (in nats) of a distribution
// implied by the supplied non-negative weights. Weights are normalised to
// shares internally, so callers may pass raw counts. Empty / all-zero input
// returns 0. Maximum is ln(k) for k equal non-zero buckets.
func ShannonEntropy(weights []float64) float64 {
	var total float64
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total == 0 {
		return 0
	}
	var h float64
	for _, w := range weights {
		if w <= 0 {
			continue
		}
		p := w / total
		h -= p * math.Log(p)
	}
	return h
}

// NormalizedEntropy returns ShannonEntropy scaled to [0, 1] by dividing by
// ln(k), where k is the number of non-empty buckets. 1.0 means the mass is
// spread perfectly evenly across the populated buckets; values near 0 mean a
// single bucket dominates. With fewer than two non-empty buckets it returns 0
// (no diversity is possible).
func NormalizedEntropy(weights []float64) float64 {
	k := 0
	for _, w := range weights {
		if w > 0 {
			k++
		}
	}
	if k < 2 {
		return 0
	}
	return ShannonEntropy(weights) / math.Log(float64(k))
}

// TopKShare returns the fraction of total mass held by the k largest values,
// in [0, 1]. k <= 0 returns 0; k >= len returns 1 (when total > 0).
func TopKShare(values []float64, k int) float64 {
	if k <= 0 || len(values) == 0 {
		return 0
	}
	xs := make([]float64, len(values))
	copy(xs, values)
	sort.Sort(sort.Reverse(sort.Float64Slice(xs)))

	var total float64
	for _, v := range xs {
		if v > 0 {
			total += v
		}
	}
	if total == 0 {
		return 0
	}
	if k > len(xs) {
		k = len(xs)
	}
	var top float64
	for i := 0; i < k; i++ {
		if xs[i] > 0 {
			top += xs[i]
		}
	}
	return top / total
}

// LargestShare is TopKShare with k=1: the single most concentrated bucket's
// fraction of the whole. This is the headline "largest-provider share" /
// "largest-ASN share" number for the concentration dashboard.
func LargestShare(values []float64) float64 { return TopKShare(values, 1) }

// DimensionConcentration bundles the concentration view of one failure-domain
// dimension (e.g. provider, ASN, region, principal) over a set of per-bucket
// weights — typically pin-incidence counts.
type DimensionConcentration struct {
	Buckets           int     // number of distinct non-empty buckets
	Gini              float64 // Gini over per-bucket weights
	NormalizedEntropy float64 // 1 = even, 0 = single bucket dominates
	LargestShare      float64 // fraction held by the biggest bucket
	Top3Share         float64 // fraction held by the three biggest buckets
}

// Concentration computes the standard concentration view over a set of
// per-bucket weights.
func Concentration(weights []float64) DimensionConcentration {
	k := 0
	for _, w := range weights {
		if w > 0 {
			k++
		}
	}
	return DimensionConcentration{
		Buckets:           k,
		Gini:              Gini(weights),
		NormalizedEntropy: NormalizedEntropy(weights),
		LargestShare:      LargestShare(weights),
		Top3Share:         TopKShare(weights, 3),
	}
}
