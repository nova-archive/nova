//go:build novasim

// novasim is the calibrated-hybrid resilience simulator for a Nova federation.
//
//	novasim calibrate    measure real per-op costs from internal/envelope + internal/ipfs
//	novasim scenario     run one resilience experiment (healing + concentration + alerts)
//	novasim sweep        find the node-count thresholds for healing within target windows
//	novasim coordinator  single-coordinator read/upload throughput ceilings
//	novasim availability multi-coordinator read-availability and the shared-hub floor
//
// Build/run with the novasim tag:
//
//	go run -tags novasim ./simulations/go/cmd/novasim <subcommand> [flags]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/nova-archive/nova/simulations/go/calib"
	"github.com/nova-archive/nova/simulations/go/model"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "calibrate":
		cmdCalibrate(os.Args[2:])
	case "scenario":
		cmdScenario(os.Args[2:])
	case "sweep":
		cmdSweep(os.Args[2:])
	case "coordinator":
		cmdCoordinator(os.Args[2:])
	case "availability":
		cmdAvailability(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `novasim — Nova federation resilience simulator (calibrated hybrid)

usage: novasim <subcommand> [flags]

subcommands:
  calibrate     measure real per-op costs (AEAD, key-unwrap, IPFS import) -> calibration.json
  scenario      one experiment: placement -> concentration/alerts -> failure -> healing
  sweep         node-count thresholds for Tier-1/Full-R healing within target windows
  coordinator   single-coordinator read-egress / upload-ingest ceilings
  availability  multi-coordinator read availability vs the shared DB/key/ingress floor

run 'novasim <subcommand> -h' for flags.
`)
}

// --- calibrate -------------------------------------------------------------

func cmdCalibrate(args []string) {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	out := fs.String("out", "calibration.json", "write measured calibration JSON here ('-' for stdout only)")
	aeadMB := fs.Int("aead-mb", 4, "AEAD throughput buffer size, MiB")
	importMB := fs.Int("import-mb", 4, "deterministic-import object size, MiB")
	importIters := fs.Int("import-iters", 40, "number of imports to time")
	_ = fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	fmt.Println("calibrating against real internal/envelope + internal/ipfs primitives…")
	cal, err := calib.Run(ctx, calib.Options{
		AEADObjectBytes:   *aeadMB << 20,
		ImportObjectBytes: *importMB << 20,
		ImportIters:       *importIters,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "calibration failed:", err)
		os.Exit(1)
	}
	printCalibration(cal)
	if *out != "-" {
		b, _ := json.MarshalIndent(cal, "", "  ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Printf("\nwrote %s\n", *out)
	}
}

func printCalibration(c model.Calibration) {
	fmt.Printf("  host                : %s (%d logical cores, measured=%v)\n", c.Host, c.Cores, c.Measured)
	fmt.Printf("  AEAD encrypt        : %s/s per core\n", bps(c.EncryptBytesPerSecPerCore))
	fmt.Printf("  AEAD decrypt        : %s/s per core\n", bps(c.DecryptBytesPerSecPerCore))
	fmt.Printf("  key unwrap          : %.2f µs/op\n", c.KeyUnwrapSeconds*1e6)
	fmt.Printf("  IPFS import         : %s/s (single op)\n", bps(c.ImportBytesPerSec))
}

// --- scenario --------------------------------------------------------------

func cmdScenario(args []string) {
	fs := flag.NewFlagSet("scenario", flag.ExitOnError)
	nodes := fs.Int("nodes", 100, "node count")
	cids := fs.Int("cids", 50000, "CID count")
	medianMB := fs.Float64("median-mb", 0.5, "median file size, MiB")
	r := fs.Int("r", 3, "replication factor")
	placement := fs.String("placement", "bandwidth", "placement: bandwidth | diversity")
	failure := fs.String("failure", "uniform", "failure: uniform | vps-bias | provider-purge | asn-purge | region-purge")
	rate := fs.Float64("rate", 0.40, "failure rate (uniform/vps-bias)")
	buckets := fs.Int("buckets", 1, "domains purged (provider/asn/region-purge)")
	dest := fs.String("dest", "uniform", "repair destination: uniform | anti-affinity")
	peers := fs.Int("peers", 0, "peer custodian federations (Phase-7 repair sources)")
	seed := fs.Int64("seed", 1, "random seed")
	_ = fs.Parse(args)

	cfg := model.DefaultScenario(*nodes)
	cfg.NumCIDs = *cids
	cfg.MedianFileBytes = *medianMB * model.MiB
	cfg.R = *r
	cfg.Seed = *seed
	cfg.Placement = buildPlacement(*placement)
	cfg.Failure = buildFailure(*failure, *rate, *buckets)
	cfg.Heal = model.DefaultHealConfig()
	cfg.Heal.TargetR = *r
	cfg.Heal.Destination = buildDestination(*dest)
	if *peers > 0 {
		cfg.Heal.PeerSources = model.BuildPeerSources(*peers, model.HighBandwidthVPS, 10_000_000)
	}

	res := model.RunScenario(cfg)
	printScenario(cfg, res)
}

func printScenario(cfg model.ScenarioConfig, r model.ScenarioResult) {
	fmt.Printf("=== scenario: N=%d CIDs=%d R=%d placement=%s failure=%s dest=%s peers=%d ===\n",
		cfg.NumNodes, cfg.NumCIDs, cfg.R, cfg.Placement.Name(), cfg.Failure.Name(), cfg.Heal.Destination.Name(), len(cfg.Heal.PeerSources))
	fmt.Printf("  survivors           : %d alive / %d dead\n", r.AliveNodes, r.DeadNodes)
	fmt.Printf("  corpus              : %s across %d CIDs\n", bytesH(r.TotalDataBytes), r.NumCIDs)
	fmt.Println("  -- steady-state concentration (the dashboard view) --")
	fmt.Printf("    pin-incidence Gini (per node) : %.3f   top-5 nodes hold %.1f%%\n", r.NodeGini, r.NodeTop5Share*100)
	printDim("provider", r.Provider)
	printDim("asn", r.ASN)
	printDim("region", r.Region)
	printDim("principal", r.Principal)
	alerts := model.DefaultAlertThresholds().Evaluate(r)
	if len(alerts) == 0 {
		fmt.Println("    concentration alerts          : none")
	} else {
		fmt.Printf("    concentration alerts          : %d (federation.concentrated would fire)\n", len(alerts))
		for _, a := range alerts {
			fmt.Printf("      - %s %s=%.3f (threshold %.2f)\n", a.Dimension, a.Metric, a.Value, a.Threshold)
		}
	}
	fmt.Println("  -- healing --")
	fmt.Printf("    zero-holders (initial)        : %d   lost-forever (final): %d\n", r.Heal.CIDsZeroHoldersInitial, r.Heal.CIDsLostForever)
	fmt.Printf("    Tier-1 at start               : %d   Tier-2 at start: %d\n", r.Heal.InitialTier1, r.Heal.InitialTier2)
	fmt.Printf("    Tier-1 cleared (>=2 copies)   : %s\n", dur(r.Heal.Tier1ClearSeconds))
	fmt.Printf("    Full R=%d restored             : %s\n", cfg.R, dur(r.Heal.FullHealSeconds))
	fmt.Printf("    healing egress                : %s\n", bytesH(r.Heal.TotalEgressBytes))
	if r.Heal.Tier1Remaining > 0 {
		fmt.Printf("    ! Tier-1 unfinished after %s: %d CIDs\n", dur(r.Heal.ElapsedSeconds), r.Heal.Tier1Remaining)
	}
}

func printDim(name string, c model.DimensionConcentration) {
	fmt.Printf("    %-30s: %d buckets, Gini %.3f, entropy %.2f, largest %.1f%%, top-3 %.1f%%\n",
		name, c.Buckets, c.Gini, c.NormalizedEntropy, c.LargestShare*100, c.Top3Share*100)
}

// --- sweep -----------------------------------------------------------------

func cmdSweep(args []string) {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	cids := fs.Int("cids", 50000, "CID count")
	medianMB := fs.Float64("median-mb", 0.5, "median file size, MiB")
	r := fs.Int("r", 3, "replication factor")
	placement := fs.String("placement", "bandwidth", "placement: bandwidth | diversity")
	failure := fs.String("failure", "uniform", "failure: uniform | vps-bias | provider-purge | asn-purge | region-purge")
	rate := fs.Float64("rate", 0.40, "failure rate (uniform/vps-bias)")
	buckets := fs.Int("buckets", 1, "domains purged (provider/asn/region-purge)")
	seed := fs.Int64("seed", 1, "random seed")
	_ = fs.Parse(args)

	sizes := []int{10, 15, 25, 40, 60, 100, 150, 250, 400, 600, 1000, 1500, 2500, 4000}
	base := model.DefaultScenario(0)
	base.NumCIDs = *cids
	base.MedianFileBytes = *medianMB * model.MiB
	base.R = *r
	base.Seed = *seed
	base.Placement = buildPlacement(*placement)
	base.Failure = buildFailure(*failure, *rate, *buckets)
	base.Network = model.DefaultNetworkConfig(0)
	base.Heal = model.DefaultHealConfig()
	base.Heal.TargetR = *r

	fmt.Printf("=== node-count thresholds: CIDs=%d median=%.1f MiB R=%d placement=%s failure=%s ===\n",
		*cids, *medianMB, *r, base.Placement.Name(), base.Failure.Name())
	fmt.Printf("    sizes tested: %v\n\n", sizes)
	targets := []struct {
		label string
		secs  int
	}{{"24 hours", 86400}, {"1 hour", 3600}, {"5 minutes", 300}}
	for _, obj := range []struct{ label, key string }{{"Tier-1 cleared", "tier1"}, {"Full R restored", "full"}} {
		fmt.Printf("  -- %s within … --\n", obj.label)
		for _, tg := range targets {
			n := model.FindThreshold(tg.secs, sizes, obj.key, base)
			if n < 0 {
				fmt.Printf("    %-10s: not achieved at any tested size\n", tg.label)
			} else {
				fmt.Printf("    %-10s: ~%d nodes\n", tg.label, n)
			}
		}
		fmt.Println()
	}
}

// --- coordinator -----------------------------------------------------------

func cmdCoordinator(args []string) {
	fs := flag.NewFlagSet("coordinator", flag.ExitOnError)
	calPath := fs.String("calib", "calibration.json", "calibration JSON (falls back to estimates if absent)")
	cores := fs.Int("cores", 8, "coordinator CPU cores")
	nicGbps := fs.Float64("nic-gbps", 1.0, "coordinator uplink, Gbps")
	_ = fs.Parse(args)

	cal := loadCalibration(*calPath)
	spec := model.DefaultCoordinatorSpec()
	spec.Cores = *cores
	spec.NICBytesPerSec = *nicGbps * 1e9 / 8.0

	fmt.Printf("=== single-coordinator ceilings (calib host=%s measured=%v; %d cores, %.0f Gbps NIC) ===\n",
		cal.Host, cal.Measured, spec.Cores, *nicGbps)
	fmt.Println("  every read decrypts at the coordinator (T1.26): donor durability does NOT raise these.")
	fmt.Println()
	fmt.Printf("  %-12s %-14s %-10s %-12s %-9s\n", "object", "read egress", "req/s", "per day", "binding")
	for _, mb := range []float64{0.05, 0.5, 2, 8, 25} {
		rc := model.ReadCeilingFor(cal, spec, mb*model.MiB)
		fmt.Printf("  %-12s %-14s %-10.0f %-12s %-9s\n",
			fmt.Sprintf("%.2f MiB", mb), bps(rc.EgressCeilingBytesPerSec)+"/s",
			rc.QPSCeiling, bytesH(model.PerDay(rc.EgressCeilingBytesPerSec)), rc.Binding)
	}
	fmt.Println()
	fmt.Printf("  %-12s %-16s %-12s %-12s\n", "object", "upload ingest", "per day", "concurrent")
	for _, mb := range []float64{0.5, 2, 8, 25} {
		uc := model.UploadCeilingFor(cal, spec, mb*model.MiB)
		fmt.Printf("  %-12s %-16s %-12s %-12.1f\n",
			fmt.Sprintf("%.2f MiB", mb), bps(uc.IngestCeilingBytesPerSec)+"/s",
			bytesH(model.PerDay(uc.IngestCeilingBytesPerSec)), uc.ConcurrentUploadsAt)
	}
}

// --- availability ----------------------------------------------------------

func cmdAvailability(args []string) {
	fs := flag.NewFlagSet("availability", flag.ExitOnError)
	coordA := fs.Float64("coord", 0.99, "per-coordinator availability")
	dbA := fs.Float64("db", 0.995, "shared Postgres authority availability")
	keyA := fs.Float64("key", 0.9999, "key-material availability")
	ingressA := fs.Float64("ingress", 0.9995, "ingress/DNS availability")
	rho := fs.Float64("rho", 0.2, "fraction of coordinator failures that are correlated")
	_ = fs.Parse(args)

	fmt.Printf("=== read availability: per-coord=%.4f db=%.4f key=%.4f ingress=%.4f rho=%.2f ===\n",
		*coordA, *dbA, *keyA, *ingressA, *rho)
	fmt.Println("  the shared DB/key/ingress hubs are a floor that more coordinators cannot lift.")
	fmt.Println()
	fmt.Printf("  %-14s %-16s %-18s %-14s\n", "coordinators", "coord tier", "end-to-end read", "downtime/yr")
	for _, k := range []int{1, 2, 3, 5} {
		tier := model.CoordinatorTierAvailability(*coordA, k, *rho)
		e2e := model.ReadAvailability(tier, *dbA, *keyA, *ingressA)
		fmt.Printf("  %-14d %-16.6f %-18.6f %-14s\n", k, tier, e2e, fmt.Sprintf("%.1f h", model.Downtime(e2e)))
	}
}

// --- helpers ---------------------------------------------------------------

func buildPlacement(name string) model.PlacementStrategy {
	switch name {
	case "diversity":
		return model.DefaultDiversityOptimized()
	default:
		return model.BandwidthWeighted{}
	}
}

func buildDestination(name string) model.DestinationPolicy {
	switch name {
	case "anti-affinity":
		return model.DefaultAntiAffinityDestination()
	default:
		return model.UniformDestination{}
	}
}

func buildFailure(name string, rate float64, buckets int) model.FailureModel {
	switch name {
	case "vps-bias":
		return model.ProfileBiasFailure{Rate: rate, Profile: model.HighBandwidthVPS.Name}
	case "provider-purge":
		return model.LargestDomainPurge{KeyName: "provider", Key: model.KeyProvider, NumBuckets: buckets}
	case "asn-purge":
		return model.LargestDomainPurge{KeyName: "asn", Key: model.KeyASN, NumBuckets: buckets}
	case "region-purge":
		return model.LargestDomainPurge{KeyName: "region", Key: model.KeyRegion, NumBuckets: buckets}
	default:
		return model.UniformFailure{Rate: rate}
	}
}

func loadCalibration(path string) model.Calibration {
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: %s not found; using estimated calibration (run `novasim calibrate`)\n", path)
		return model.DefaultCalibration(runtime.NumCPU())
	}
	var c model.Calibration
	if err := json.Unmarshal(b, &c); err != nil {
		fmt.Fprintf(os.Stderr, "note: %s unreadable (%v); using estimates\n", path, err)
		return model.DefaultCalibration(runtime.NumCPU())
	}
	return c
}

func bps(b float64) string { return bytesH(b) }
func bytesH(b float64) string {
	switch {
	case b >= model.TiB:
		return fmt.Sprintf("%.2f TiB", b/model.TiB)
	case b >= model.GiB:
		return fmt.Sprintf("%.2f GiB", b/model.GiB)
	case b >= model.MiB:
		return fmt.Sprintf("%.2f MiB", b/model.MiB)
	case b >= model.KiB:
		return fmt.Sprintf("%.1f KiB", b/model.KiB)
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func dur(seconds int) string {
	if seconds < 0 {
		return "— (not reached)"
	}
	switch {
	case seconds < 600:
		return fmt.Sprintf("%d s", seconds)
	case seconds < 86400:
		return fmt.Sprintf("%d min", seconds/60)
	default:
		return fmt.Sprintf("%.1f days", float64(seconds)/86400.0)
	}
}
