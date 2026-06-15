package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// nestedMap builds a nested map[string]any from a dotted key path and leaf value.
// E.g. nestedMap("uploads.limits.max_concurrent_global", 8) produces:
//
//	{"uploads": {"limits": {"max_concurrent_global": 8}}}
func nestedMap(dotted string, leaf any) map[string]any {
	parts := strings.Split(dotted, ".")
	root := map[string]any{}
	cur := root
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = leaf
		} else {
			next := map[string]any{}
			cur[p] = next
			cur = next
		}
	}
	return root
}

// parseLeafValue coerces a string value into the most specific JSON type that
// fits. When rawJSON is true the value is already JSON and is unmarshalled
// directly. Otherwise: "true"/"false" → bool; integer string → int64; else
// string.
func parseLeafValue(s string, rawJSON bool) (any, error) {
	if rawJSON {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return nil, fmt.Errorf("--json: invalid JSON %q: %w", s, err)
		}
		return v, nil
	}
	if s == "true" {
		return true, nil
	}
	if s == "false" {
		return false, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	return s, nil
}

// configSetResponse is the partial shape of PATCH /api/v1/admin/config's 200 body.
type configSetResponse struct {
	Version         uint64   `json:"version"`
	RestartRequired []string `json:"restart_required"`
}

// --------------------------------------------------------------------------
// Subcommand: config get
// --------------------------------------------------------------------------

func cmdConfigGet(args []string) error {
	fs := flag.NewFlagSet("config get", flag.ContinueOnError)
	effects := fs.Bool("effects", false, "also print per-field effect and source information")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := getJSON(newClient(), c.BaseURL+"/api/v1/admin/config", c.AccessToken)
	if err != nil {
		return fmt.Errorf("config get request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode config get response: %w", err)
		}
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("format config get response: %w", err)
		}
		fmt.Println(string(out))
		if *effects {
			// Best-effort: extract the fields map and print per-key effect/source.
			var envelope struct {
				Fields map[string]struct {
					Effect string `json:"effect"`
					Source string `json:"source"`
				} `json:"fields"`
			}
			if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Fields) > 0 {
				fmt.Fprintln(os.Stdout, "\nfield effects:")
				for k, f := range envelope.Fields {
					fmt.Fprintf(os.Stdout, "  %s: effect=%s source=%s\n", k, f.Effect, f.Source)
				}
			}
		}
		return nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator role required")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("config get failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Subcommand: config set
// --------------------------------------------------------------------------

func cmdConfigSet(args []string) error {
	fs := flag.NewFlagSet("config set", flag.ContinueOnError)
	rawJSON := fs.Bool("json", false, "treat value as raw JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: novactl config set <dotted.key> <value> [--json]")
		return errors.New("config set requires <key> and <value>")
	}
	key := rest[0]
	val := rest[1]

	leaf, err := parseLeafValue(val, *rawJSON)
	if err != nil {
		return err
	}

	body := nestedMap(key, leaf)

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := patchJSON(newClient(), c.BaseURL+"/api/v1/admin/config", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("config set request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var sr configSetResponse
		if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
			return fmt.Errorf("decode config set response: %w", err)
		}
		fmt.Printf("config updated (version %d)\n", sr.Version)
		if len(sr.RestartRequired) > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: restart the coordinator for: %s\n",
				strings.Join(sr.RestartRequired, ", "))
		}
		return nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator role required")
	default:
		body2, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("config set failed (HTTP %d): %s", resp.StatusCode, string(body2))
	}
}

// --------------------------------------------------------------------------
// Subcommand: config apply
// --------------------------------------------------------------------------

func cmdConfigApply(args []string) error {
	fs := flag.NewFlagSet("config apply", flag.ContinueOnError)
	configFile := fs.String("config-file", "", "path to YAML config file (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configFile == "" {
		fmt.Fprintln(os.Stderr, "usage: novactl config apply --config-file <path>")
		return errors.New("config apply requires --config-file")
	}

	data, err := os.ReadFile(*configFile)
	if err != nil {
		return fmt.Errorf("config apply: read %s: %w", *configFile, err)
	}
	var body map[string]any
	if err := yaml.Unmarshal(data, &body); err != nil {
		return fmt.Errorf("config apply: parse %s: %w", *configFile, err)
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := putJSON(newClient(), c.BaseURL+"/api/v1/admin/config", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("config apply request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode config apply response: %w", err)
		}
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("format config apply response: %w", err)
		}
		fmt.Println(string(out))
		// Also check for restart_required in the response.
		var sr configSetResponse
		if err := json.Unmarshal(raw, &sr); err == nil && len(sr.RestartRequired) > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: restart the coordinator for: %s\n",
				strings.Join(sr.RestartRequired, ", "))
		}
		return nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator role required")
	default:
		body2, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("config apply failed (HTTP %d): %s", resp.StatusCode, string(body2))
	}
}

// --------------------------------------------------------------------------
// Subcommand group: config
// --------------------------------------------------------------------------

func cmdConfig(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "get":
		return cmdConfigGet(args[1:])
	case "set":
		return cmdConfigSet(args[1:])
	case "apply":
		return cmdConfigApply(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}
