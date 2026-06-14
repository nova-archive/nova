package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// WriteAtomic marshals cfg to YAML, self-checks it round-trips through the
// loader, then writes it to path via temp-file + fsync + rename so a reader
// never observes a partial file and a crash leaves the prior file intact.
func WriteAtomic(path string, cfg *Config) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if _, err := LoadFromBytes(b); err != nil { // self-validation, mirrors setup/render.go
		return fmt.Errorf("config: refusing to write invalid yaml: %w", err)
	}
	dir := filepath.Dir(path)
	var rnd [8]byte
	_, _ = rand.Read(rnd[:])
	tmp := filepath.Join(dir, "."+filepath.Base(path)+".tmp-"+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("config: open temp: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("config: write temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("config: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename: %w", err)
	}
	if d, err := os.Open(dir); err == nil { // best-effort dir fsync for durability
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// ToMap renders cfg to a yaml-keyed map (the canonical shape for the admin API
// and for merging — keys match operator.yaml, not Go field names).
func ToMap(cfg *Config) (map[string]any, error) {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// FromMap re-parses a yaml-keyed map back into a validated *Config (the single
// load path: validate + privacy preset + upload defaults).
func FromMap(m map[string]any) (*Config, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return LoadFromBytes(b)
}

// MergePatch deep-merges patch into a copy of base (maps recurse; scalars and
// slices replace). Returns the merged map; inputs are not mutated.
func MergePatch(base, patch map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, pv := range patch {
		if pm, ok := pv.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = MergePatch(bm, pm)
				continue
			}
		}
		out[k] = pv
	}
	return out
}
