package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// deleteRequest sends an HTTP DELETE to url with the given bearer token.
func deleteRequest(client *http.Client, url, bearerToken string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	return client.Do(req)
}

// expiresAtFromDuration converts a Go duration string (e.g. "720h") relative to
// now into an RFC3339 timestamp string. An empty s returns ("", nil) to signal
// "no expiry". Go's time.ParseDuration does NOT support "d" (days) — use hours
// (e.g. "720h" for 30 days).
func expiresAtFromDuration(s string, now time.Time) (string, error) {
	if s == "" {
		return "", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return "", fmt.Errorf("invalid --expires duration %q (use e.g. 720h for 30 days; 'd' is not supported): %w", s, err)
	}
	return now.Add(d).UTC().Format(time.RFC3339), nil
}

// --------------------------------------------------------------------------
// Subcommand: upload-token create
// --------------------------------------------------------------------------

func cmdUploadTokenCreate(args []string) error {
	fs := flag.NewFlagSet("upload-token create", flag.ContinueOnError)
	label := fs.String("label", "", "human-readable label for the token")
	collection := fs.String("collection", "", "collection UUID to scope this token to")
	product := fs.String("product", "", "allowed product type: image/video/audio/archive/document/raw")
	maxFileSize := fs.Int64("max-file-size", 0, "maximum file size in bytes (0 = server default)")
	expires := fs.String("expires", "", "token lifetime as a Go duration, e.g. 720h (30 days); 'd' unit not supported")
	if err := fs.Parse(args); err != nil {
		return err
	}

	expiresAt, err := expiresAtFromDuration(*expires, time.Now())
	if err != nil {
		return err
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	body := map[string]any{}
	if *label != "" {
		body["label"] = *label
	}
	if *collection != "" {
		body["collection_id"] = *collection
	}
	if *product != "" {
		body["product"] = *product
	}
	if *maxFileSize != 0 {
		body["max_file_size"] = *maxFileSize
	}
	if expiresAt != "" {
		body["expires_at"] = expiresAt
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/upload-tokens", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("upload-token create request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode upload-token create response: %w", err)
		}
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("format upload-token create response: %w", err)
		}
		fmt.Println(string(out))
		fmt.Fprintln(os.Stderr, "Save the token now — it is shown only once and cannot be retrieved later.")
		return nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator role required")
	default:
		body2, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload-token create failed (HTTP %d): %s", resp.StatusCode, string(body2))
	}
}

// --------------------------------------------------------------------------
// Subcommand: upload-token list
// --------------------------------------------------------------------------

func cmdUploadTokenList(args []string) error {
	_ = args // no flags

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := getJSON(newClient(), c.BaseURL+"/api/v1/admin/upload-tokens", c.AccessToken)
	if err != nil {
		return fmt.Errorf("upload-token list request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode upload-token list response: %w", err)
		}
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("format upload-token list response: %w", err)
		}
		fmt.Println(string(out))
		return nil
	case http.StatusUnauthorized:
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		return errors.New("forbidden — operator role required")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload-token list failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Subcommand: upload-token revoke
// --------------------------------------------------------------------------

func cmdUploadTokenRevoke(args []string) error {
	// id is the first positional arg; flags follow.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: novactl upload-token revoke <id> [--no-confirm]")
		return errors.New("missing required argument: <id>")
	}
	id := args[0]
	fs := flag.NewFlagSet("upload-token revoke", flag.ContinueOnError)
	noConfirm := fs.Bool("no-confirm", false, "skip the confirmation prompt")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if !*noConfirm {
		if !confirm("Revoke this upload token? Embedded widgets using it will stop working. [y/N] ") {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := deleteRequest(newClient(), c.BaseURL+"/api/v1/admin/upload-tokens/"+id, c.AccessToken)
	if err != nil {
		return fmt.Errorf("upload-token revoke request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent:
		_, _ = io.Copy(io.Discard, resp.Body)
		fmt.Println("revoked")
		return nil
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("token not found or already revoked")
	case http.StatusBadRequest:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bad request: %s", string(body))
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator role required")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload-token revoke failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Subcommand group: upload-token
// --------------------------------------------------------------------------

func cmdUploadToken(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "create":
		return cmdUploadTokenCreate(args[1:])
	case "list":
		return cmdUploadTokenList(args[1:])
	case "revoke":
		return cmdUploadTokenRevoke(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}
