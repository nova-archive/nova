package ipfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ipfs/boxo/files"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	kuboconfig "github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	coreiface "github.com/ipfs/kubo/core/coreiface"
	"github.com/ipfs/kubo/core/coreiface/options"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo/fsrepo"
	mh "github.com/multiformats/go-multihash"
)

// pluginsOnce guards plugin loading. Kubo's plugin system installs
// datastore implementations into a package-level registry on Initialize+
// Inject — calling those twice in one process errors with "already have
// a datastore named ...". sync.Once is the simplest correct fix; the
// load happens against the FIRST repo path passed to NewEmbedded, which
// is fine because the plugin set (badgerds, flatfs, levelds, pebbleds)
// is process-wide and independent of which repo it was first loaded for.
var (
	pluginsOnce sync.Once
	pluginsErr  error
)

func loadPluginsOnce(repoPath string) error {
	pluginsOnce.Do(func() {
		plugins, err := loader.NewPluginLoader(repoPath)
		if err != nil {
			pluginsErr = fmt.Errorf("plugin loader: %w", err)
			return
		}
		if err := plugins.Initialize(); err != nil {
			pluginsErr = fmt.Errorf("plugin init: %w", err)
			return
		}
		if err := plugins.Inject(); err != nil {
			pluginsErr = fmt.Errorf("plugin inject: %w", err)
			return
		}
	})
	return pluginsErr
}

// EmbeddedOptions configures the in-process Kubo node.
type EmbeddedOptions struct {
	// RepoPath is the directory holding the Kubo fsrepo. Must be
	// writable; will be initialised if empty.
	RepoPath string

	// Mode selects the hardening profile.
	Mode Mode

	// SwarmKeyPath is the file containing /key/swarm/psk/1.0.0/.
	// Required in ModePrivate; ignored in ModePublicArchivalDHT.
	SwarmKeyPath string

	// Online controls whether libp2p starts. Tests typically pass false.
	// Production passes true.
	Online bool
}

// EmbeddedBackend is the in-process Kubo implementation of Backend.
type EmbeddedBackend struct {
	node    *core.IpfsNode
	api     coreiface.CoreAPI
	repoDir string
}

// Compile-time interface check.
var _ Backend = (*EmbeddedBackend)(nil)

// NewEmbedded initialises (if necessary), opens, validates, and boots
// the embedded Kubo node. The returned backend MUST be Closed on
// shutdown.
//
// NewEmbedded refuses to return a backend until ValidateConfig has
// passed against the loaded Kubo config — this is the refuse-to-start
// floor.
func NewEmbedded(ctx context.Context, opts EmbeddedOptions) (*EmbeddedBackend, error) {
	// Ensure plugins (datastore, etc.) are loaded exactly once per
	// process. Kubo's plugin Inject installs into a package-level
	// registry; calling it twice errors.
	if err := loadPluginsOnce(opts.RepoPath); err != nil {
		return nil, fmt.Errorf("ipfs embedded: %w", err)
	}

	if !fsrepo.IsInitialized(opts.RepoPath) {
		cfg, err := kuboconfig.Init(io.Discard, 2048)
		if err != nil {
			return nil, fmt.Errorf("ipfs embedded: config init: %w", err)
		}
		applyHardeningDefaults(cfg, opts.Mode)
		if err := fsrepo.Init(opts.RepoPath, cfg); err != nil {
			return nil, fmt.Errorf("ipfs embedded: fsrepo init: %w", err)
		}
	}

	repo, err := fsrepo.Open(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: fsrepo open: %w", err)
	}

	loadedCfg, err := repo.Config()
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("ipfs embedded: load config: %w", err)
	}
	ourCfg := translateKuboConfig(loadedCfg)
	if err := ValidateConfig(ourCfg, opts.Mode, opts.SwarmKeyPath); err != nil {
		_ = repo.Close()
		return nil, err
	}

	// In ModePrivate, install the operator's swarm key into the repo so
	// libp2p's private-network protector loads the PSK. Kubo reads the
	// PSK *only* from <repo>/swarm.key; if it is absent, an online node
	// silently joins the PUBLIC libp2p network, defeating the central
	// donor-blind/private-federation guarantee. ValidateConfig has
	// already confirmed the source file exists and is non-empty.
	//
	// TODO(M3): when Online mode is wired in the coordinator, also set
	// LIBP2P_FORCE_PNET=1 as defense-in-depth so libp2p refuses to dial
	// if the PSK ever goes missing at runtime.
	if opts.Mode == ModePrivate {
		if err := installSwarmKey(opts.RepoPath, opts.SwarmKeyPath); err != nil {
			_ = repo.Close()
			return nil, fmt.Errorf("ipfs embedded: %w", err)
		}
	}

	node, err := core.NewNode(ctx, &core.BuildCfg{
		Repo:   repo,
		Online: opts.Online,
	})
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("ipfs embedded: new node: %w", err)
	}

	api, err := coreapi.NewCoreAPI(node)
	if err != nil {
		_ = node.Close()
		return nil, fmt.Errorf("ipfs embedded: coreapi: %w", err)
	}

	return &EmbeddedBackend{node: node, api: api, repoDir: opts.RepoPath}, nil
}

// installSwarmKey copies the operator's swarm key into the Kubo repo at
// <repoPath>/swarm.key, where libp2p's private-network protector reads
// it. It overwrites any existing key so operator key rotation takes
// effect on restart. The file is written 0600 — it gates federation
// membership and must not be world-readable.
func installSwarmKey(repoPath, swarmKeyPath string) error {
	data, err := os.ReadFile(swarmKeyPath)
	if err != nil {
		return fmt.Errorf("read swarm key %s: %w", swarmKeyPath, err)
	}
	dst := filepath.Join(repoPath, "swarm.key")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("write swarm key %s: %w", dst, err)
	}
	return nil
}

// applyHardeningDefaults mutates a fresh Kubo config to satisfy
// KUBO_HARDENING.md in the requested mode. ValidateConfig is the
// authoritative gate; this function exists so first-time init produces
// a config that already passes the validator.
func applyHardeningDefaults(cfg *kuboconfig.Config, mode Mode) {
	cfg.Discovery.MDNS.Enabled = false
	cfg.Bootstrap = []string{}
	cfg.Addresses.API = kuboconfig.Strings{"/ip4/127.0.0.1/tcp/5001"}
	cfg.Addresses.Gateway = kuboconfig.Strings{"/ip4/127.0.0.1/tcp/8080"}
	cfg.Swarm.DisableNatPortMap = true

	if mode == ModePrivate {
		// Routing.Type and Provide.Strategy are pointer-y in modern
		// Kubo configs; set via the spec-mandated values.
		cfg.Routing.Type = kuboconfig.NewOptionalString("none")
		cfg.Provide.Strategy = kuboconfig.NewOptionalString("")
	}
	// ModePublicArchivalDHT: leave Kubo defaults for routing.
}

// translateKuboConfig converts the loaded Kubo config into our pure-Go
// KuboConfig struct (the type ValidateConfig accepts). We translate
// rather than import Kubo's types into the validator to keep the
// validator stable across Kubo upgrades.
func translateKuboConfig(c *kuboconfig.Config) KuboConfig {
	out := KuboConfig{
		Discovery: DiscoveryConfig{MDNS: MDNSConfig{Enabled: c.Discovery.MDNS.Enabled}},
		Swarm:     SwarmConfig{DisableNatPortMap: c.Swarm.DisableNatPortMap},
	}
	if c.Routing.Type != nil {
		out.Routing.Type = c.Routing.Type.WithDefault("")
	}
	if c.Provide.Strategy != nil {
		// Kubo ≥0.36 merged Provider/Reprovider into the single Provide
		// config; mirror its strategy into both validator fields so both
		// KUBO_HARDENING.md rows stay independently enforced.
		strategy := c.Provide.Strategy.WithDefault("")
		out.Provider.Strategy = strategy
		out.Reprovider.Strategy = strategy
	}
	if len(c.Addresses.API) > 0 {
		out.Addresses.API = c.Addresses.API[0]
	}
	if len(c.Addresses.Gateway) > 0 {
		out.Addresses.Gateway = c.Addresses.Gateway[0]
	}
	out.Bootstrap = append(out.Bootstrap, c.Bootstrap...)
	return out
}

// AddDeterministic dispatches between the raw-codec shortcut and the
// dag-pb UnixFS pipeline based on envelope size, per IPFS_IMPORT_RULES.md.
func (b *EmbeddedBackend) AddDeterministic(ctx context.Context, envelope []byte) (AddResult, error) {
	size := int64(len(envelope))
	if ShouldUseRawCodec(size) {
		return b.addRaw(ctx, envelope)
	}
	return b.addDagPB(ctx, envelope)
}

func (b *EmbeddedBackend) addRaw(ctx context.Context, envelope []byte) (AddResult, error) {
	stat, err := b.api.Block().Put(ctx, bytes.NewReader(envelope),
		options.Block.Format(CodecRaw),
		options.Block.Hash(mh.SHA2_256, -1),
		options.Block.Pin(true),
	)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: block put: %w", err)
	}
	c := stat.Path().RootCid()
	return AddResult{
		CID:          c,
		EnvelopeSize: int64(len(envelope)),
		Codec:        CodecRaw,
		Blocks:       []Block{{CID: c, Index: 0, Size: len(envelope)}},
		MerkleRoot:   c,
	}, nil
}

func (b *EmbeddedBackend) addDagPB(ctx context.Context, envelope []byte) (AddResult, error) {
	resPath, err := b.api.Unixfs().Add(ctx,
		files.NewBytesFile(envelope),
		options.Unixfs.CidVersion(1),
		options.Unixfs.Hash(mh.SHA2_256),
		options.Unixfs.RawLeaves(true),
		options.Unixfs.Chunker(ChunkerSpec),
		options.Unixfs.Layout(options.BalancedLayout),
		options.Unixfs.Pin(true, ""),
	)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: unixfs add: %w", err)
	}
	rootCid := resPath.RootCid()

	blocks, err := b.enumerateBlocks(ctx, rootCid)
	if err != nil {
		return AddResult{}, fmt.Errorf("ipfs embedded: enumerate blocks: %w", err)
	}
	return AddResult{
		CID:          rootCid,
		EnvelopeSize: int64(len(envelope)),
		Codec:        CodecDagPB,
		Blocks:       blocks,
		MerkleRoot:   rootCid,
	}, nil
}

// enumerateBlocks walks the DAG rooted at root and emits a Block per
// leaf in DAG-traversal order. Used by AddDeterministic to populate
// blob_blocks rows.
func (b *EmbeddedBackend) enumerateBlocks(ctx context.Context, root cid.Cid) ([]Block, error) {
	out := []Block{}
	if err := b.walkLeaves(ctx, root, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *EmbeddedBackend) walkLeaves(ctx context.Context, c cid.Cid, out *[]Block) error {
	// Raw-codec CIDs are leaves by definition (no links); for those we
	// fetch the block directly and record its size. For dag-pb CIDs,
	// we descend through the DAG via ipld.DAGService.Get().
	if c.Type() == cid.Raw {
		blk, err := b.node.Blockstore.Get(ctx, c)
		if err != nil {
			return fmt.Errorf("walk raw block %s: %w", c, err)
		}
		*out = append(*out, Block{CID: c, Index: len(*out), Size: len(blk.RawData())})
		return nil
	}
	node, err := b.node.DAG.Get(ctx, c)
	if err != nil {
		return fmt.Errorf("walk dag-pb %s: %w", c, err)
	}
	links := node.Links()
	if len(links) == 0 {
		// Internal node with no links (rare); record as leaf.
		size, _ := node.Size()
		*out = append(*out, Block{CID: c, Index: len(*out), Size: int(size)})
		return nil
	}
	for _, l := range links {
		if err := b.walkLeaves(ctx, l.Cid, out); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the (reassembled) bytes for c. For raw-codec CIDs this
// is the single block; for dag-pb CIDs this is the UnixFS file content.
func (b *EmbeddedBackend) Get(ctx context.Context, c cid.Cid) (io.ReadCloser, error) {
	node, err := b.api.Unixfs().Get(ctx, path.FromCid(c))
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: unixfs get: %w", err)
	}
	file, ok := node.(files.File)
	if !ok {
		_ = node.Close()
		return nil, fmt.Errorf("ipfs embedded: get returned non-file node for cid %s", c)
	}
	return file, nil
}

func (b *EmbeddedBackend) Has(ctx context.Context, c cid.Cid) (bool, error) {
	// Iterate the pinset and look for our CID. Pin().Ls in modern Kubo
	// is a caller-provided channel; we buffer modestly so we don't
	// block the sender if our CID is found early.
	pins := make(chan coreiface.Pin, 32)
	errCh := make(chan error, 1)
	lsCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		errCh <- b.api.Pin().Ls(lsCtx, pins, options.Pin.Ls.Recursive())
	}()
	for p := range pins {
		if p.Path().RootCid().Equals(c) {
			cancel()
			// Drain remaining sends so the producer can exit.
			for range pins {
			}
			<-errCh
			return true, nil
		}
	}
	if err := <-errCh; err != nil {
		return false, fmt.Errorf("ipfs embedded: pin ls: %w", err)
	}
	return false, nil
}

func (b *EmbeddedBackend) Pin(ctx context.Context, c cid.Cid) error {
	return b.api.Pin().Add(ctx, path.FromCid(c))
}

func (b *EmbeddedBackend) Unpin(ctx context.Context, c cid.Cid) error {
	return b.api.Pin().Rm(ctx, path.FromCid(c))
}

func (b *EmbeddedBackend) BlockstoreHas(ctx context.Context, c cid.Cid) (bool, error) {
	// Check the local blockstore directly (faster + bypasses pin/path
	// resolution). The audit subsystem uses this to verify block
	// presence independent of pin status.
	return b.node.Blockstore.Has(ctx, c)
}

func (b *EmbeddedBackend) BlockGet(ctx context.Context, c cid.Cid) ([]byte, error) {
	r, err := b.api.Block().Get(ctx, path.FromCid(c))
	if err != nil {
		return nil, fmt.Errorf("ipfs embedded: block get %s: %w", c, err)
	}
	return io.ReadAll(r)
}

func (b *EmbeddedBackend) Close(_ context.Context) error {
	if b.node == nil {
		return nil
	}
	err := b.node.Close()
	b.node = nil
	b.api = nil
	return err
}

// Health reports backend liveness for /readyz. For the embedded backend
// this means: the Kubo node has been constructed, has not been closed,
// and its HTTP API surface is wired. No I/O — the call is intentionally
// cheap so /readyz can be polled at ~1 Hz without amortized cost. A
// remote backend (Phase 2) would ping the loopback API instead.
func (b *EmbeddedBackend) Health(ctx context.Context) error {
	if b.node == nil || b.api == nil {
		return errors.New("ipfs embedded: backend closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
