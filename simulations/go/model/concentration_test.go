//go:build novasim

package model

import (
	"math"
	"testing"
)

func approx(t *testing.T, got, want, tol float64, name string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.6f, want %.6f (±%.6f)", name, got, want, tol)
	}
}

func TestGiniKnownValues(t *testing.T) {
	// Perfect equality -> 0.
	approx(t, Gini([]float64{5, 5, 5, 5}), 0, 1e-9, "Gini(equal)")
	// All mass in one bucket of n -> (n-1)/n.
	approx(t, Gini([]float64{0, 0, 0, 7}), 3.0/4.0, 1e-9, "Gini(one-of-4)")
	approx(t, Gini([]float64{9, 0, 0, 0, 0}), 4.0/5.0, 1e-9, "Gini(one-of-5)")
	// Empty / all-zero -> 0 (no mass).
	approx(t, Gini(nil), 0, 1e-9, "Gini(nil)")
	approx(t, Gini([]float64{0, 0, 0}), 0, 1e-9, "Gini(zeros)")
	// Two-point distribution {1,3}: mean=2, MD = (|1-1|+|1-3|+|3-1|+|3-3|)/4 = 1,
	// Gini = MD/(2*mean) = 1/4.
	approx(t, Gini([]float64{1, 3}), 0.25, 1e-9, "Gini({1,3})")
}

func TestShannonEntropyKnownValues(t *testing.T) {
	// Uniform over k buckets -> ln(k).
	approx(t, ShannonEntropy([]float64{1, 1, 1, 1}), math.Log(4), 1e-9, "H(uniform-4)")
	approx(t, ShannonEntropy([]float64{3, 3, 3}), math.Log(3), 1e-9, "H(uniform-3 counts)")
	// All in one bucket -> 0.
	approx(t, ShannonEntropy([]float64{0, 0, 5}), 0, 1e-9, "H(degenerate)")
	approx(t, ShannonEntropy(nil), 0, 1e-9, "H(nil)")
}

func TestNormalizedEntropyKnownValues(t *testing.T) {
	// Uniform -> 1.0 regardless of k.
	approx(t, NormalizedEntropy([]float64{1, 1, 1, 1}), 1.0, 1e-9, "Hn(uniform-4)")
	approx(t, NormalizedEntropy([]float64{2, 2}), 1.0, 1e-9, "Hn(uniform-2)")
	// Single populated bucket -> 0 (no diversity possible).
	approx(t, NormalizedEntropy([]float64{0, 9, 0}), 0, 1e-9, "Hn(single)")
	// Skewed is strictly between 0 and 1.
	hn := NormalizedEntropy([]float64{90, 5, 5})
	if !(hn > 0 && hn < 1) {
		t.Errorf("Hn(skewed) = %.4f, want strictly in (0,1)", hn)
	}
}

func TestTopKShareAndLargest(t *testing.T) {
	vals := []float64{10, 30, 60} // total 100
	approx(t, LargestShare(vals), 0.6, 1e-9, "LargestShare")
	approx(t, TopKShare(vals, 2), 0.9, 1e-9, "Top2Share")
	approx(t, TopKShare(vals, 3), 1.0, 1e-9, "Top3Share")
	approx(t, TopKShare(vals, 5), 1.0, 1e-9, "TopK(k>n)")
	approx(t, TopKShare(vals, 0), 0, 1e-9, "TopK(0)")
	approx(t, TopKShare(nil, 2), 0, 1e-9, "TopK(nil)")
}

func TestConcentrationBundle(t *testing.T) {
	// Even spread across 4 providers.
	even := Concentration([]float64{25, 25, 25, 25})
	if even.Buckets != 4 {
		t.Errorf("even.Buckets = %d, want 4", even.Buckets)
	}
	approx(t, even.Gini, 0, 1e-9, "even.Gini")
	approx(t, even.NormalizedEntropy, 1.0, 1e-9, "even.NormalizedEntropy")
	approx(t, even.LargestShare, 0.25, 1e-9, "even.LargestShare")

	// One provider dominates.
	skew := Concentration([]float64{97, 1, 1, 1})
	if skew.Gini <= even.Gini {
		t.Errorf("skew.Gini (%.4f) should exceed even.Gini (%.4f)", skew.Gini, even.Gini)
	}
	if skew.NormalizedEntropy >= even.NormalizedEntropy {
		t.Errorf("skew.NormalizedEntropy (%.4f) should be below even (%.4f)", skew.NormalizedEntropy, even.NormalizedEntropy)
	}
	approx(t, skew.LargestShare, 0.97, 1e-9, "skew.LargestShare")
}
