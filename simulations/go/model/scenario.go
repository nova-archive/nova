//go:build novasim

package model

// ScenarioConfig fully specifies one resilience experiment.
type ScenarioConfig struct {
	NumNodes        int
	NumCIDs         int
	MedianFileBytes float64
	R               int
	Network         NetworkConfig
	Placement       PlacementStrategy
	Failure         FailureModel
	Heal            HealConfig
	Seed            int64
}

// DefaultScenario returns the standard experiment for a given node count: the
// orchestrator_resilience.py defaults (50k CIDs, 0.5 MiB median, R=3, 40%
// uniform failure, bandwidth-weighted placement, uniform destination).
func DefaultScenario(numNodes int) ScenarioConfig {
	hc := DefaultHealConfig()
	return ScenarioConfig{
		NumNodes:        numNodes,
		NumCIDs:         50000,
		MedianFileBytes: 0.5 * MiB,
		R:               3,
		Network:         DefaultNetworkConfig(numNodes),
		Placement:       BandwidthWeighted{},
		Failure:         UniformFailure{Rate: 0.40},
		Heal:            hc,
		Seed:            1,
	}
}

// ScenarioResult bundles healing outcomes with the steady-state concentration
// view (computed on the live placement BEFORE the failure event — the picture
// an operator's dashboard would alert on).
type ScenarioResult struct {
	NumNodes       int
	AliveNodes     int
	DeadNodes      int
	NumCIDs        int
	TotalDataBytes float64

	Heal HealResult

	NodeGini      float64
	NodeTop5Share float64
	Provider      DimensionConcentration
	ASN           DimensionConcentration
	Region        DimensionConcentration
	Principal     DimensionConcentration
}

// RunScenario executes one experiment end-to-end.
func RunScenario(cfg ScenarioConfig) ScenarioResult {
	if cfg.Network.NumNodes == 0 {
		cfg.Network = DefaultNetworkConfig(cfg.NumNodes)
	}
	cfg.Network.NumNodes = cfg.NumNodes
	if cfg.Placement == nil {
		cfg.Placement = BandwidthWeighted{}
	}
	if cfg.Heal.Destination == nil {
		cfg.Heal = DefaultHealConfig()
	}
	if cfg.Heal.TargetR == 0 {
		cfg.Heal.TargetR = cfg.R
	}

	nodes := BuildNetwork(cfg.Network, cfg.Seed)
	sizes := FileSizes(cfg.NumCIDs, cfg.MedianFileBytes, cfg.Seed)
	pins := AssignStorage(nodes, sizes, cfg.R, cfg.Placement, cfg.Seed)

	// Steady-state concentration snapshot (pre-failure).
	nodeInc := PinIncidenceByNode(nodes)
	res := ScenarioResult{
		NumNodes:      cfg.NumNodes,
		NumCIDs:       cfg.NumCIDs,
		NodeGini:      Gini(nodeInc),
		NodeTop5Share: TopKShare(nodeInc, 5),
		Provider:      Concentration(PinIncidenceBy(nodes, KeyProvider)),
		ASN:           Concentration(PinIncidenceBy(nodes, KeyASN)),
		Region:        Concentration(PinIncidenceBy(nodes, KeyRegion)),
		Principal:     Concentration(PinIncidenceBy(nodes, KeyPrincipal)),
	}
	for _, s := range sizes {
		res.TotalDataBytes += s
	}

	dead := cfg.Failure.Apply(nodes, randFor(cfg.Seed))
	res.DeadNodes = len(dead)
	res.AliveNodes = cfg.NumNodes - len(dead)

	res.Heal = Heal(nodes, pins, sizes, cfg.Heal)
	return res
}

// ConcentrationAlert is one fired diversity/homogeneity alert — the "alert,
// not prevent" output. Dimension is e.g. "provider"; Metric and Threshold
// describe what tripped.
type ConcentrationAlert struct {
	Dimension string
	Metric    string
	Value     float64
	Threshold float64
}

// AlertThresholds parameterises the concentration alerting. Defaults follow
// the design doc: alert when any single failure domain holds too large a share
// of replicas, or when a dimension's spread (normalized entropy) collapses.
type AlertThresholds struct {
	LargestShareWarn      float64 // e.g. 0.30
	NormalizedEntropyWarn float64 // e.g. 0.50 (below = concentrated)
}

// DefaultAlertThresholds returns the design-doc defaults.
func DefaultAlertThresholds() AlertThresholds {
	return AlertThresholds{LargestShareWarn: 0.30, NormalizedEntropyWarn: 0.50}
}

// EvaluateAlerts checks each domain dimension against the thresholds and
// returns the alerts that fired. This NEVER blocks placement — it is the
// operator-facing signal (federation.concentrated / .homogeneous).
func (t AlertThresholds) Evaluate(res ScenarioResult) []ConcentrationAlert {
	var out []ConcentrationAlert
	check := func(dim string, c DimensionConcentration) {
		if c.LargestShare > t.LargestShareWarn {
			out = append(out, ConcentrationAlert{dim, "largest-share", c.LargestShare, t.LargestShareWarn})
		}
		if c.Buckets >= 2 && c.NormalizedEntropy < t.NormalizedEntropyWarn {
			out = append(out, ConcentrationAlert{dim, "normalized-entropy", c.NormalizedEntropy, t.NormalizedEntropyWarn})
		}
	}
	check("provider", res.Provider)
	check("asn", res.ASN)
	check("region", res.Region)
	check("principal", res.Principal)
	return out
}

// FindThreshold returns the smallest node count from sizes at which the
// objective ("tier1" or "full") completes within targetSeconds, or -1 if none
// do. base is a template; only NumNodes and Network.NumNodes vary.
func FindThreshold(targetSeconds int, sizes []int, objective string, base ScenarioConfig) int {
	for _, n := range sizes {
		cfg := base
		cfg.NumNodes = n
		cfg.Network = base.Network
		cfg.Network.NumNodes = n
		res := RunScenario(cfg)
		var t int
		if objective == "full" {
			t = res.Heal.FullHealSeconds
		} else {
			t = res.Heal.Tier1ClearSeconds
		}
		if t >= 0 && t <= targetSeconds {
			return n
		}
	}
	return -1
}
