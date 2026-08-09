package bootstrap

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stage implements failure-atomicity WITH RESPECT TO ACTIVATION (D-M7.2-2a).
//
// `federation init` writes across at least three Docker volumes (nova-fedpki,
// nova-secrets, nova-config). There is no transaction spanning them, so
// claiming "atomic" would be false. What IS achievable, and what matters:
//
//  1. stage into a generation-scoped directory, never over active material;
//  2. fsync each file and its parent;
//  3. validate the COMPLETE staged federation as a unit;
//  4. activate by rename;
//  5. write operator.yaml last — the activation boundary, before which the
//     coordinator's next boot behaves as though init never ran;
//  6. an interrupted run leaves only inert staged material, and a re-run
//     inventories, discards orphans and resumes.
//
// That makes the idempotence claim meaningful under power loss rather than
// only meaning "running it twice after a success is harmless".
type Stage struct {
	Root  string // volume root, e.g. the nova-fedpki mount
	GenID string // this run's generation
	dir   string // <Root>/.staging/<GenID>
	files []string
}

const stagingDirName = ".staging"

// NewStage creates a generation-scoped staging directory under root.
func NewStage(root string) (*Stage, error) {
	gen := fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), os.Getpid())
	dir := filepath.Join(root, stagingDirName, gen)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("stage: create %s: %w", dir, err)
	}
	return &Stage{Root: root, GenID: gen, dir: dir}, nil
}

// Dir is the staging directory for this generation.
func (s *Stage) Dir() string { return s.dir }

// Put writes a staged file, fsyncing the file and its parent directory.
func (s *Stage) Put(rel string, data []byte, perm os.FileMode) error {
	dst := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("stage: mkdir for %s: %w", rel, err)
	}
	if err := writeFileSync(dst, data, perm); err != nil {
		return fmt.Errorf("stage: write %s: %w", rel, err)
	}
	if err := fsyncDir(filepath.Dir(dst)); err != nil {
		return fmt.Errorf("stage: fsync dir for %s: %w", rel, err)
	}
	s.files = append(s.files, rel)
	return nil
}

// Has reports whether rel was staged in this generation.
func (s *Stage) Has(rel string) bool {
	for _, f := range s.files {
		if f == rel {
			return true
		}
	}
	return false
}

// Files returns the staged relative paths, sorted.
func (s *Stage) Files() []string {
	out := append([]string(nil), s.files...)
	sort.Strings(out)
	return out
}

// Validate runs v over the staged tree. Nothing has been activated yet, so a
// validation failure leaves the active federation exactly as it was.
func (s *Stage) Validate(v func(fs.FS) error) error {
	if v == nil {
		return nil
	}
	if err := v(os.DirFS(s.dir)); err != nil {
		return fmt.Errorf("stage: validation failed, nothing activated: %w", err)
	}
	return nil
}

// Activate renames staged files into their final locations, fsyncing each
// destination directory. targets maps a staged relative path to a final
// absolute path. Files not present in targets stay staged and are discarded
// with the generation.
func (s *Stage) Activate(targets map[string]string) error {
	rels := make([]string, 0, len(targets))
	for rel := range targets {
		rels = append(rels, rel)
	}
	sort.Strings(rels) // deterministic order aids debugging a partial activate

	dirs := map[string]struct{}{}
	for _, rel := range rels {
		src := filepath.Join(s.dir, rel)
		dst := targets[rel]
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("stage: mkdir for %s: %w", dst, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("stage: activate %s -> %s: %w", rel, dst, err)
		}
		dirs[filepath.Dir(dst)] = struct{}{}
	}
	for d := range dirs {
		if err := fsyncDir(d); err != nil {
			return fmt.Errorf("stage: fsync %s: %w", d, err)
		}
	}
	return nil
}

// Discard removes this generation's staging directory.
func (s *Stage) Discard() error {
	return os.RemoveAll(s.dir)
}

// DiscardOrphans removes every staging generation under root except keep.
// An interrupted run leaves inert staged material; this is what makes a re-run
// converge rather than accumulate.
func DiscardOrphans(root, keep string) error {
	base := filepath.Join(root, stagingDirName)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
			return fmt.Errorf("stage: discard orphan %s: %w", e.Name(), err)
		}
	}
	return nil
}

// writeFileSync writes data and fsyncs the file before returning, so a crash
// after this point cannot leave a torn file.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// OpenFile honours perm only on creation; enforce it for an existing file.
	return os.Chmod(path, perm)
}

// fsyncDir durably records a directory's entries.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !strings.Contains(err.Error(), "invalid argument") {
		// Some filesystems refuse fsync on a directory handle; that is not a
		// durability failure we can act on.
		return err
	}
	return nil
}

// WriteFileAtomic writes a file via temp -> fsync -> rename -> dir fsync.
// Used for operator.yaml, the activation boundary, which is written LAST and
// must never be observed half-written.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}
