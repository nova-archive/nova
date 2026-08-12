package release

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nova-archive/nova/internal/config"
)

// Config-key compatibility (P2-M7.3, D-M7.3-20 preflight step 4).
//
// # Warn, never fail, and NEVER rewrite
//
// `config/operator_yaml.go` parses with plain yaml.Unmarshal and no
// KnownFields, so an unknown key is silently ignored today. Turning that into a
// hard error in the same release that introduces the check would break
// deployments at the worst possible moment — during an upgrade — for a
// misspelling that has been harmless for months. So preflight WARNS, and strict
// parsing arrives a release later, after operators have had a cycle of
// warnings.
//
// Unknown values are PRESERVED. The walk reads a yaml.Node tree and never
// re-marshals the document, so a key this binary does not recognize survives
// untouched — including one a NEWER release added, which matters because
// preflight runs from the target binary against the running deployment's file.

// KeyFinding is one observation about the operator's configuration.
type KeyFinding struct {
	// Path is the dotted key, e.g. "federation.listen_addr".
	Path string
	// Kind is "unknown" (this binary does not recognize it) or "missing"
	// (the target requires it and the file does not set it).
	Kind string
	// Detail explains what it means for the upgrade.
	Detail string
}

// KnownConfigKeys returns every dotted key path the target binary understands,
// derived by reflection over config.Config's yaml tags.
//
// Reflection rather than a hand-maintained list: a list would be a second
// source of truth about the config surface, and the first thing to rot.
func KnownConfigKeys() []string {
	set := map[string]bool{}
	walkType(reflect.TypeOf(config.Config{}), "", set)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func walkType(t reflect.Type, prefix string, out map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		out[path] = true

		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			walkType(ft, path, out)
		}
	}
}

// InspectConfigKeys reads an operator.yaml and reports keys the target binary
// does not recognize. It returns findings, never an error for an unknown key.
//
// A parse failure IS an error: a file this binary cannot read is a real
// blocker, not a warning.
func InspectConfigKeys(yamlBytes []byte) ([]KeyFinding, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &doc); err != nil {
		return nil, fmt.Errorf("operator.yaml does not parse: %w", err)
	}
	if len(doc.Content) == 0 {
		return nil, nil // an empty file sets nothing, which is legal
	}

	known := map[string]bool{}
	for _, k := range KnownConfigKeys() {
		known[k] = true
	}

	var findings []KeyFinding
	walkNode(doc.Content[0], "", known, &findings)
	sort.Slice(findings, func(i, j int) bool { return findings[i].Path < findings[j].Path })
	return findings, nil
}

func walkNode(n *yaml.Node, prefix string, known map[string]bool, out *[]KeyFinding) {
	if n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, val := n.Content[i], n.Content[i+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + key.Value
		}
		if !known[path] {
			*out = append(*out, KeyFinding{
				Path: path, Kind: "unknown",
				Detail: "this release does not recognize the key. It is IGNORED, not rewritten, " +
					"and your file is left untouched — but check the spelling, because a " +
					"misspelled setting has never taken effect.",
			})
			// Do not descend: everything under an unknown key is unknown too,
			// and listing each leaf would bury the one line that matters.
			continue
		}
		walkNode(val, path, known, out)
	}
}
