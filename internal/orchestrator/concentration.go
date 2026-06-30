package orchestrator

import (
	"context"
	"math"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/notify"
)

// gini is the Gini coefficient of a distribution of non-negative counts:
// G = Σ_i Σ_j |x_i − x_j| / (2 n Σx). It is 0 for ≤1 element, an all-zero
// distribution, or a perfectly uniform one; it approaches 1 as pins concentrate on
// a single node (D-M5-10).
func gini(x []int64) float64 {
	n := len(x)
	if n <= 1 {
		return 0
	}
	var sum int64
	for _, v := range x {
		sum += v
	}
	if sum == 0 {
		return 0
	}
	var num int64
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			d := x[i] - x[j]
			if d < 0 {
				d = -d
			}
			num += d
		}
	}
	return float64(num) / (2 * float64(n) * float64(sum))
}

// DimMetrics holds the per-dimension concentration stats.
type DimMetrics struct {
	LargestShare      float64
	LargestValue      string
	TopKShare         float64
	NormalizedEntropy float64
	Groups            int
}

// dimensionMetrics computes share + normalized-entropy stats over per-group pin
// counts keyed by collapsed dimension value (the caller has already collapsed
// unverified/NULL into "unknown", D-M5-10). k is clamped to the group count;
// normalized entropy is 0 for a single group (avoids the ln(1)=0 divide).
func dimensionMetrics(groups map[string]int64, k int) DimMetrics {
	n := len(groups)
	if n == 0 {
		return DimMetrics{}
	}
	type kv struct {
		key   string
		count int64
	}
	pairs := make([]kv, 0, n)
	var total int64
	for key, c := range groups {
		pairs = append(pairs, kv{key, c})
		total += c
	}
	if total == 0 {
		return DimMetrics{Groups: n}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count != pairs[j].count {
			return pairs[i].count > pairs[j].count
		}
		return pairs[i].key < pairs[j].key
	})
	if k > n {
		k = n
	}
	if k < 1 {
		k = 1
	}
	var topK int64
	for i := 0; i < k; i++ {
		topK += pairs[i].count
	}
	entropy := 0.0
	for _, p := range pairs {
		if p.count == 0 {
			continue
		}
		frac := float64(p.count) / float64(total)
		entropy -= frac * math.Log(frac)
	}
	norm := 0.0
	if n > 1 {
		norm = entropy / math.Log(float64(n))
	}
	return DimMetrics{
		LargestShare:      float64(pairs[0].count) / float64(total),
		LargestValue:      pairs[0].key,
		TopKShare:         float64(topK) / float64(total),
		NormalizedEntropy: norm,
		Groups:            n,
	}
}

// Concentration is the corpus-wide concentration snapshot.
type Concentration struct {
	NodeGini float64
	Dims     map[string]DimMetrics // dimension name → metrics
}

const unknownBucket = "unknown"

func collapse(v string) string {
	if v == "" {
		return unknownBucket
	}
	return v
}

// ComputeConcentration reads acked placement over nodes and computes per-node Gini
// + per-dimension share/entropy, collapsing unverified/NULL dimension values into a
// single "unknown" bucket BEFORE grouping so placement and metrics agree (D-M5-10).
func ComputeConcentration(ctx context.Context, pool *pgxpool.Pool, topK int) (Concentration, error) {
	rows, err := gen.New(pool).ListAckedNodeDimensions(ctx)
	if err != nil {
		return Concentration{}, err
	}
	nodeCounts := make([]int64, 0, len(rows))
	dims := map[string]map[string]int64{
		"failure_domain":  {},
		"donor_principal": {},
		"provider":        {},
		"asn":             {},
		"region":          {},
	}
	for _, r := range rows {
		nodeCounts = append(nodeCounts, r.Pins)
		dims["failure_domain"][collapse(r.FailureDomain)] += r.Pins
		dims["donor_principal"][collapse(r.DonorPrincipal)] += r.Pins
		dims["provider"][collapse(r.Provider)] += r.Pins
		dims["asn"][collapse(r.Asn)] += r.Pins
		dims["region"][collapse(r.Region)] += r.Pins
	}
	out := Concentration{NodeGini: gini(nodeCounts), Dims: map[string]DimMetrics{}}
	for name, groups := range dims {
		out.Dims[name] = dimensionMetrics(groups, topK)
	}
	return out, nil
}

// ConcentrationThresholds tunes the federation.concentrated / .homogeneous signals.
type ConcentrationThresholds struct {
	LargestShareMax      float64 // > ⇒ concentrated
	NormalizedEntropyMin float64 // < ⇒ homogeneous
}

// EmitConcentration computes concentration and emits federation.concentrated (a
// dimension's largest share exceeds the threshold) and/or federation.homogeneous (a
// dimension's normalized entropy is below the threshold), scoped per D-M5-9a. The
// Notifier applies once-per-window suppression. Placement is never refused for
// homogeneity — these are operator signals only.
func EmitConcentration(ctx context.Context, pool *pgxpool.Pool, n notify.Notifier, topK int, th ConcentrationThresholds) error {
	c, err := ComputeConcentration(ctx, pool, topK)
	if err != nil {
		return err
	}
	for dim, m := range c.Dims {
		if m.Groups == 0 {
			continue
		}
		if m.LargestShare > th.LargestShareMax {
			n.Emit(ctx, notify.Event{
				Type:     "federation.concentrated",
				ScopeKey: dim + ":" + m.LargestValue,
				Payload: map[string]any{
					"dimension": dim, "value": m.LargestValue,
					"largest_share": m.LargestShare, "top_k_share": m.TopKShare, "node_gini": c.NodeGini,
				},
			})
		}
		if m.Groups > 1 && m.NormalizedEntropy < th.NormalizedEntropyMin {
			n.Emit(ctx, notify.Event{
				Type:     "federation.homogeneous",
				ScopeKey: dim,
				Payload: map[string]any{
					"dimension": dim, "normalized_entropy": m.NormalizedEntropy, "groups": m.Groups,
				},
			})
		}
	}
	return nil
}
