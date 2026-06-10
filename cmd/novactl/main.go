// Package main is the Nova command-line client.
//
// Subcommands:
//
//	novactl auth login [--url <base>] [--username <u>]
//	novactl auth whoami
//	novactl auth logout
//	novactl signed-url sign --path <p> [--ttl <secs>] --aud <origin>
//	novactl moderation quarantine <cid> [--case <id>] [--reason <s>] [--tombstone-after 14d] [--legal-hold]
//	novactl moderation takedown <cid> [--case <id>] [--reason <s>] [--no-confirm]
//	novactl moderation clear-legal-hold <cid> [--case-id <ref>] [--reason <s>] [--no-confirm]
//	novactl moderation restore <cid> [--reason <s>]
//	novactl moderation list [--per-page <n>]
//	novactl keys rotate-master --from v1 --to v2 [--no-confirm]
//	novactl keys status
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/nova-archive/nova/internal/db"
	"github.com/nova-archive/nova/internal/db/gen"
	"github.com/nova-archive/nova/internal/setup"
)

const defaultBaseURL = "http://localhost:8080"

const httpTimeout = 30 * time.Second

// credentials is the local credential cache stored at ~/.config/nova/credentials.json.
type credentials struct {
	BaseURL      string `json:"base_url"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	KID          string `json:"kid"`
}

// credsPath returns the path to the credentials file, honouring $XDG_CONFIG_HOME.
// It also creates the parent directory with mode 0o700 if it does not exist.
func credsPath() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("home dir: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	dir := filepath.Join(configHome, "nova")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create config dir: %w", err)
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// writeCredentials marshals c to JSON and writes it to path with mode 0o600.
// If the file already existed with looser permissions it is tightened.
func writeCredentials(path string, c credentials) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	// Ensure the file has exactly 0600 even if umask was permissive.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod credentials: %w", err)
	}
	return nil
}

// readCredentials reads and unmarshals the credential file at path.
func readCredentials(path string) (credentials, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, fmt.Errorf("read credentials: %w", err)
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return credentials{}, fmt.Errorf("parse credentials: %w", err)
	}
	return c, nil
}

// --------------------------------------------------------------------------
// HTTP helpers
// --------------------------------------------------------------------------

func newClient() *http.Client {
	return &http.Client{Timeout: httpTimeout}
}

func postJSON(client *http.Client, url string, body any, bearerToken string) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return client.Do(req)
}

func getJSON(client *http.Client, url, bearerToken string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return client.Do(req)
}

// apiError is the JSON error envelope the coordinator returns.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func readAPIError(r io.Reader) apiError {
	var ae apiError
	_ = json.NewDecoder(r).Decode(&ae)
	return ae
}

// --------------------------------------------------------------------------
// Auth login response shape (snake_case, coordinator-issued)
// --------------------------------------------------------------------------

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	KID          string `json:"kid"`
}

// authConfigResponse is returned by GET /api/v1/auth/config.
type authConfigResponse struct {
	Mode      string `json:"mode"`
	IssuerURL string `json:"issuer_url"`
	ClientID  string `json:"client_id"`
}

// userResponse is returned by GET /api/v1/users/me.
type userResponse struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// --------------------------------------------------------------------------
// Subcommand: auth login
// --------------------------------------------------------------------------

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("auth login", flag.ContinueOnError)
	url := fs.String("url", defaultBaseURL, "Nova coordinator base URL")
	username := fs.String("username", "", "Nova username")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *username == "" {
		fmt.Print("Username: ")
		var u string
		if _, err := fmt.Scanln(&u); err != nil {
			return fmt.Errorf("read username: %w", err)
		}
		*username = u
	}

	fmt.Print("Password: ")
	passBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println() // newline after hidden input
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	password := string(passBytes)

	client := newClient()
	loginURL := *url + "/api/v1/auth/login"
	resp, err := postJSON(client, loginURL, map[string]string{
		"username": *username,
		"password": password,
	}, "")
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var tr tokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			return fmt.Errorf("decode login response: %w", err)
		}
		path, err := credsPath()
		if err != nil {
			return err
		}
		c := credentials{
			BaseURL:      *url,
			AccessToken:  tr.AccessToken,
			RefreshToken: tr.RefreshToken,
			KID:          tr.KID,
		}
		if err := writeCredentials(path, c); err != nil {
			return err
		}
		fmt.Println("Logged in successfully.")
		return nil

	case http.StatusUnauthorized:
		_ = readAPIError(resp.Body)
		fmt.Fprintln(os.Stderr, "invalid credentials")
		os.Exit(1)

	case http.StatusServiceUnavailable:
		_, _ = io.Copy(io.Discard, resp.Body)
		fmt.Fprintln(os.Stderr, "rate limited, try again shortly")
		os.Exit(1)

	case http.StatusNotFound:
		// External OIDC mode active — read config and guide the user.
		ae := readAPIError(resp.Body)
		if ae.Code == "external_oidc_active" {
			if err := printExternalOIDCGuidance(client, *url); err != nil {
				fmt.Fprintf(os.Stderr, "external OIDC mode active (could not fetch config: %v)\n", err)
			}
			os.Exit(1)
		}
		return fmt.Errorf("unexpected 404: %s", ae.Message)

	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

func printExternalOIDCGuidance(client *http.Client, baseURL string) error {
	resp, err := getJSON(client, baseURL+"/api/v1/auth/config", "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var cfg authConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "This Nova instance uses external OIDC authentication.")
	if cfg.IssuerURL != "" {
		fmt.Fprintf(os.Stderr, "Identity provider: %s\n", cfg.IssuerURL)
	}
	fmt.Fprintln(os.Stderr, "Obtain a token from your IdP and supply it manually.")
	return nil
}

// --------------------------------------------------------------------------
// Subcommand: auth whoami
// --------------------------------------------------------------------------

func cmdWhoami() error {
	path, err := credsPath()
	if err != nil {
		return err
	}
	c, err := readCredentials(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("not logged in — run: novactl auth login")
		}
		return err
	}

	client := newClient()
	u, retryErr := fetchMe(client, c)
	if retryErr != nil {
		// Try token refresh once.
		refreshed, refreshErr := doRefresh(client, c)
		if refreshErr != nil {
			return fmt.Errorf("session expired and refresh failed: %w", refreshErr)
		}
		// Store the rotated pair.
		if err := writeCredentials(path, refreshed); err != nil {
			return err
		}
		c = refreshed
		u, err = fetchMe(client, c)
		if err != nil {
			return fmt.Errorf("whoami after refresh: %w", err)
		}
	}

	fmt.Printf("id:         %s\n", u.ID)
	fmt.Printf("email:      %s\n", u.Email)
	fmt.Printf("role:       %s\n", u.Role)
	fmt.Printf("created_at: %s\n", u.CreatedAt)
	fmt.Printf("updated_at: %s\n", u.UpdatedAt)
	return nil
}

// fetchMe calls GET /api/v1/users/me and returns an error on 401.
func fetchMe(client *http.Client, c credentials) (userResponse, error) {
	resp, err := getJSON(client, c.BaseURL+"/api/v1/users/me", c.AccessToken)
	if err != nil {
		return userResponse{}, fmt.Errorf("users/me request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return userResponse{}, errors.New("unauthorized")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return userResponse{}, fmt.Errorf("users/me failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var u userResponse
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return userResponse{}, fmt.Errorf("decode users/me: %w", err)
	}
	return u, nil
}

// doRefresh exchanges the refresh token for a new pair.
func doRefresh(client *http.Client, c credentials) (credentials, error) {
	resp, err := postJSON(client, c.BaseURL+"/api/v1/auth/refresh", map[string]string{
		"refresh_token": c.RefreshToken,
	}, "")
	if err != nil {
		return credentials{}, fmt.Errorf("refresh request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return credentials{}, fmt.Errorf("refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return credentials{}, fmt.Errorf("decode refresh response: %w", err)
	}
	return credentials{
		BaseURL:      c.BaseURL,
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		KID:          tr.KID,
	}, nil
}

// --------------------------------------------------------------------------
// Subcommand: auth logout
// --------------------------------------------------------------------------

func cmdLogout() error {
	path, err := credsPath()
	if err != nil {
		return err
	}
	c, err := readCredentials(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("Already logged out.")
			return nil
		}
		return err
	}

	client := newClient()
	resp, err := postJSON(client, c.BaseURL+"/api/v1/auth/logout", map[string]string{
		"refresh_token": c.RefreshToken,
	}, "")
	if err != nil {
		return fmt.Errorf("logout request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove credentials: %w", err)
	}
	fmt.Println("Logged out.")
	return nil
}

// --------------------------------------------------------------------------
// Subcommand: signed-url sign
// --------------------------------------------------------------------------

// cmdSignedURLSign POSTs /api/v1/admin/signed-urls/sign with the cached bearer
// token and prints the resulting absolute signed URL.
func cmdSignedURLSign(args []string) error {
	fs := flag.NewFlagSet("signed-url sign", flag.ContinueOnError)
	path := fs.String("path", "", "content path to sign (/blob/{cid} or /i/{cid}/...)")
	ttl := fs.Int("ttl", 3600, "URL lifetime in seconds")
	aud := fs.String("aud", "", "embedding origin, e.g. https://example.com")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *aud == "" {
		return errors.New("both --path and --aud are required")
	}

	credPath, err := credsPath()
	if err != nil {
		return err
	}
	c, err := readCredentials(credPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("not logged in — run: novactl auth login")
		}
		return err
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/signed-urls/sign", map[string]any{
		"path":        *path,
		"ttl_seconds": *ttl,
		"aud":         *aud,
	}, c.AccessToken)
	if err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		var sr struct {
			URL string `json:"url"`
			KID string `json:"kid"`
			Exp int64  `json:"exp"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
			return fmt.Errorf("decode sign response: %w", err)
		}
		fmt.Println(c.BaseURL + sr.URL)
		return nil
	case http.StatusUnauthorized:
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		return errors.New("forbidden — signing requires the operator or moderator role")
	case http.StatusBadRequest:
		return fmt.Errorf("invalid request: %s", readAPIError(resp.Body).Message)
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sign failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Moderation helpers
// --------------------------------------------------------------------------

// confirm prints prompt to stderr, reads a line from stdin, and returns true
// iff the response is "y" or "Y".
func confirm(prompt string) bool {
	fmt.Fprint(os.Stderr, prompt)
	var line string
	_, _ = fmt.Fscanln(os.Stdin, &line)
	return line == "y" || line == "Y"
}

// loadCreds reads the stored credentials and returns a user-friendly error if
// the file is absent.
func loadCreds() (credentials, error) {
	path, err := credsPath()
	if err != nil {
		return credentials{}, err
	}
	c, err := readCredentials(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentials{}, errors.New("not logged in — run: novactl auth login")
		}
		return credentials{}, err
	}
	return c, nil
}

// handleModerationResponse handles the common HTTP status codes returned by
// moderation endpoints and prints a result or error message.
func handleModerationResponse(resp *http.Response) error {
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		body, _ := io.ReadAll(resp.Body)
		fmt.Println(string(body))
		return nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("forbidden — operator/moderator role required")
	case http.StatusNotFound:
		_, _ = io.Copy(io.Discard, resp.Body)
		return errors.New("not found")
	case http.StatusConflict:
		ae := readAPIError(resp.Body)
		return fmt.Errorf("%s", ae.Message)
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("request failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Subcommand: moderation quarantine
// --------------------------------------------------------------------------

func cmdModerationQuarantine(args []string) error {
	// CID is the first positional arg; flags follow.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: novactl moderation quarantine <cid> [--case <id>] [--reason <s>] [--tombstone-after 14d] [--legal-hold]")
		return errors.New("missing required argument: <cid>")
	}
	cid := args[0]
	fs := flag.NewFlagSet("moderation quarantine", flag.ContinueOnError)
	caseID := fs.String("case", "", "moderation case ID")
	reason := fs.String("reason", "", "reason for quarantine")
	tombstoneAfter := fs.String("tombstone-after", "", "tombstone delay, e.g. 14d")
	legalHold := fs.Bool("legal-hold", false, "place a legal hold (also sets rule to severe_content)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	body := map[string]any{
		"cid":        cid,
		"reason":     *reason,
		"legal_hold": *legalHold,
	}
	if *caseID != "" {
		body["case_id"] = *caseID
	}
	if *tombstoneAfter != "" {
		body["tombstone_after"] = *tombstoneAfter
	}
	if *legalHold {
		body["rule"] = "severe_content"
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/moderation/quarantine", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("quarantine request: %w", err)
	}
	return handleModerationResponse(resp)
}

// --------------------------------------------------------------------------
// Subcommand: moderation takedown
// --------------------------------------------------------------------------

func cmdModerationTakedown(args []string) error {
	// CID is the first positional arg; flags follow.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: novactl moderation takedown <cid> [--case <id>] [--reason <s>] [--no-confirm]")
		return errors.New("missing required argument: <cid>")
	}
	cid := args[0]
	fs := flag.NewFlagSet("moderation takedown", flag.ContinueOnError)
	caseID := fs.String("case", "", "moderation case ID")
	reason := fs.String("reason", "", "reason for takedown")
	noConfirm := fs.Bool("no-confirm", false, "skip the destructive-action confirmation prompt")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if !*noConfirm {
		if !confirm("This permanently crypto-shreds the blob. Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	body := map[string]any{
		"cid":    cid,
		"reason": *reason,
	}
	if *caseID != "" {
		body["case_id"] = *caseID
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/moderation/takedown", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("takedown request: %w", err)
	}
	return handleModerationResponse(resp)
}

// --------------------------------------------------------------------------
// Subcommand: moderation clear-legal-hold
// --------------------------------------------------------------------------

func cmdModerationClearLegalHold(args []string) error {
	// CID is the first positional arg; flags follow.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: novactl moderation clear-legal-hold <cid> [--case-id <ref>] [--reason <s>] [--no-confirm]")
		return errors.New("missing required argument: <cid>")
	}
	cid := args[0]
	fs := flag.NewFlagSet("moderation clear-legal-hold", flag.ContinueOnError)
	caseRef := fs.String("case-id", "", "case reference")
	reason := fs.String("reason", "", "reason for releasing the hold")
	noConfirm := fs.Bool("no-confirm", false, "skip the destructive-action confirmation prompt")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if !*noConfirm {
		if !confirm("This releases the legal hold and allows tombstone. Continue? [y/N] ") {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	body := map[string]any{
		"cid":    cid,
		"reason": *reason,
	}
	if *caseRef != "" {
		body["case_ref"] = *caseRef
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/moderation/clear-legal-hold", body, c.AccessToken)
	if err != nil {
		return fmt.Errorf("clear-legal-hold request: %w", err)
	}
	if resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return errors.New("requires operator role")
	}
	return handleModerationResponse(resp)
}

// --------------------------------------------------------------------------
// Subcommand: moderation restore
// --------------------------------------------------------------------------

func cmdModerationRestore(args []string) error {
	// CID is the first positional arg; flags follow.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(os.Stderr, "usage: novactl moderation restore <cid> [--reason <s>]")
		return errors.New("missing required argument: <cid>")
	}
	cid := args[0]
	fs := flag.NewFlagSet("moderation restore", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason for restoring the blob")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/moderation/restore", map[string]any{
		"cid":    cid,
		"reason": *reason,
	}, c.AccessToken)
	if err != nil {
		return fmt.Errorf("restore request: %w", err)
	}
	return handleModerationResponse(resp)
}

// --------------------------------------------------------------------------
// Subcommand: moderation list
// --------------------------------------------------------------------------

func cmdModerationList(args []string) error {
	fs := flag.NewFlagSet("moderation list", flag.ContinueOnError)
	perPage := fs.Int("per-page", 20, "number of results per page")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/api/v1/admin/moderation/queue?per_page=%d", c.BaseURL, *perPage)
	resp, err := getJSON(newClient(), url, c.AccessToken)
	if err != nil {
		return fmt.Errorf("list request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var raw json.RawMessage
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			return fmt.Errorf("decode list response: %w", err)
		}
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return fmt.Errorf("format list response: %w", err)
		}
		fmt.Println(string(out))
		return nil
	case http.StatusUnauthorized:
		return errors.New("unauthorized — run: novactl auth login")
	case http.StatusForbidden:
		return errors.New("forbidden — operator/moderator role required")
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("list failed (HTTP %d): %s", resp.StatusCode, string(body))
	}
}

// --------------------------------------------------------------------------
// Subcommand group: moderation
// --------------------------------------------------------------------------

func cmdModeration(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "quarantine":
		return cmdModerationQuarantine(args[1:])
	case "takedown":
		return cmdModerationTakedown(args[1:])
	case "clear-legal-hold":
		return cmdModerationClearLegalHold(args[1:])
	case "restore":
		return cmdModerationRestore(args[1:])
	case "list":
		return cmdModerationList(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}

// --------------------------------------------------------------------------
// Keys response structs
// --------------------------------------------------------------------------

// rotateMasterResp is the 202 body returned by POST /api/v1/admin/keys/rotate-master.
type rotateMasterResp struct {
	From             string `json:"from"`
	To               string `json:"to"`
	TotalDEKs        int    `json:"total_deks"`
	TotalSigningKeys int    `json:"total_signing_keys"`
	Status           any    `json:"status"`
}

// progressResp is the in_progress object within rotationStatusResp (null when idle).
type progressResp struct {
	From                 string `json:"from"`
	RemainingDEKs        int    `json:"remaining_deks"`
	RemainingSigningKeys int    `json:"remaining_signing_keys"`
	Stalled              bool   `json:"stalled"`
	StallReason          string `json:"stall_reason"`
}

// versionInfoResp is one entry in the versions array of rotationStatusResp.
type versionInfoResp struct {
	Label        string  `json:"label"`
	State        string  `json:"state"`
	DEKCount     int     `json:"dek_count"`
	SigningCount int     `json:"signing_count"`
	RetiredAt    *string `json:"retired_at"`
}

// rotationStatusResp is the body returned by GET /api/v1/admin/keys/rotation-status.
type rotationStatusResp struct {
	Active     string            `json:"active"`
	InProgress *progressResp     `json:"in_progress"`
	Versions   []versionInfoResp `json:"versions"`
}

// --------------------------------------------------------------------------
// Subcommand: keys rotate-master
// --------------------------------------------------------------------------

func cmdKeysRotateMaster(args []string) error {
	fs := flag.NewFlagSet("keys rotate-master", flag.ContinueOnError)
	from := fs.String("from", "", "retiring version label (e.g. v1)")
	to := fs.String("to", "", "new active version label (e.g. v2)")
	noConfirm := fs.Bool("no-confirm", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Validate required flags before touching credentials or network.
	if *from == "" || *to == "" {
		return errors.New("rotate-master requires --from and --to")
	}

	c, err := loadCreds()
	if err != nil {
		return err
	}

	if !*noConfirm {
		prompt := fmt.Sprintf("Re-wrap every key from %s to %s? This re-encrypts all DEKs and signing keys. [y/N] ", *from, *to)
		if !confirm(prompt) {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}

	resp, err := postJSON(newClient(), c.BaseURL+"/api/v1/admin/keys/rotate-master",
		map[string]any{"from_version": *from, "to_version": *to}, c.AccessToken)
	if err != nil {
		return fmt.Errorf("rotate-master request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rotate-master failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var rmr rotateMasterResp
	if err := json.NewDecoder(resp.Body).Decode(&rmr); err != nil {
		return fmt.Errorf("decode rotate-master response: %w", err)
	}

	fmt.Printf("rotation started: %s → %s (%d DEKs, %d signing keys); polling…\n",
		*from, *to, rmr.TotalDEKs, rmr.TotalSigningKeys)

	return pollRotation(c, *from)
}

// pollRotation loops calling the rotation-status endpoint, printing progress
// until in_progress is nil (complete) or stalled (error).
func pollRotation(c credentials, from string) error {
	for {
		resp, err := getJSON(newClient(), c.BaseURL+"/api/v1/admin/keys/rotation-status", c.AccessToken)
		if err != nil {
			return fmt.Errorf("rotation-status request: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("rotation-status failed (HTTP %d): %s", resp.StatusCode, string(body))
		}

		var st rotationStatusResp
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			return fmt.Errorf("decode rotation-status: %w", err)
		}

		if st.InProgress == nil {
			fmt.Println("rotation complete.")
			return nil
		}
		if st.InProgress.Stalled {
			return fmt.Errorf("rotation STALLED: %s (restore the %q master key and restart the coordinator)",
				st.InProgress.StallReason, from)
		}
		fmt.Printf("  remaining: %d DEKs, %d signing keys\n",
			st.InProgress.RemainingDEKs, st.InProgress.RemainingSigningKeys)
		time.Sleep(2 * time.Second)
	}
}

// --------------------------------------------------------------------------
// Subcommand: keys status
// --------------------------------------------------------------------------

func cmdKeysStatus(args []string) error {
	_ = args // no flags for status

	c, err := loadCreds()
	if err != nil {
		return err
	}

	resp, err := getJSON(newClient(), c.BaseURL+"/api/v1/admin/keys/rotation-status", c.AccessToken)
	if err != nil {
		return fmt.Errorf("rotation-status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rotation-status failed (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var st rotationStatusResp
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return fmt.Errorf("decode rotation-status: %w", err)
	}

	fmt.Printf("active: %s\n", st.Active)
	fmt.Printf("%-12s %-12s %10s %14s\n", "label", "state", "dek_count", "signing_count")
	for _, v := range st.Versions {
		fmt.Printf("%-12s %-12s %10d %14d\n", v.Label, v.State, v.DEKCount, v.SigningCount)
	}
	if st.InProgress != nil {
		fmt.Printf("in_progress: from=%s remaining_deks=%d remaining_signing_keys=%d stalled=%v\n",
			st.InProgress.From, st.InProgress.RemainingDEKs, st.InProgress.RemainingSigningKeys, st.InProgress.Stalled)
	}

	return nil
}

// --------------------------------------------------------------------------
// Subcommand group: keys
// --------------------------------------------------------------------------

func cmdKeys(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: novactl keys <rotate-master|status>")
	}
	switch args[0] {
	case "rotate-master":
		return cmdKeysRotateMaster(args[1:])
	case "status":
		return cmdKeysStatus(args[1:])
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

// --------------------------------------------------------------------------
// Subcommand: setup
// --------------------------------------------------------------------------

// loadAnswersFile reads and validates a YAML answers file from path.
func loadAnswersFile(path string) (setup.Answers, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return setup.Answers{}, fmt.Errorf("setup: read %s: %w", path, err)
	}
	var a setup.Answers
	if err := yaml.Unmarshal(data, &a); err != nil {
		return setup.Answers{}, fmt.Errorf("setup: parse %s: %w", path, err)
	}
	if err := a.Validate(); err != nil {
		return setup.Answers{}, err
	}
	return a, nil
}

// commitSetup generates the first-run secrets, prints the master key + its
// fingerprint to out (so a headless operator can back them up), then commits.
func commitSetup(ctx context.Context, a setup.Answers, p setup.Paths, uc setup.UserCreator, out io.Writer) error {
	s, err := setup.GenerateSecrets()
	if err != nil {
		return err
	}
	fp, err := setup.Fingerprint(s.MasterKeyHex)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "MASTER KEY (back this up securely — it cannot be recovered):\n  %s\n  fingerprint: %s\n", s.MasterKeyHex, fp)
	return setup.Commit(ctx, a, p, s, uc)
}

// cmdSetup is the `novactl setup` subcommand. It runs BEFORE the coordinator
// serves — postgres must be up and migrations applied.
func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	interactive := fs.Bool("interactive", false, "prompt for each field on stdin")
	configFile := fs.String("config-file", "", "path to YAML answers file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	configDir := os.Getenv("NOVA_CONFIG_DIR")
	if configDir == "" {
		configDir = "/etc/nova"
	}
	secretsDir := os.Getenv("NOVA_SECRETS_DIR")
	if secretsDir == "" {
		secretsDir = "/run/secrets"
	}
	paths := setup.Paths{
		ConfigDir:  configDir,
		SecretsDir: secretsDir,
		Sentinel:   filepath.Join(configDir, ".bootstrap-complete"),
	}

	ctx := context.Background()

	switch {
	case *configFile != "":
		a, err := loadAnswersFile(*configFile)
		if err != nil {
			return err
		}
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return errors.New("setup: DATABASE_URL must be set")
		}
		pool, err := db.Open(ctx, dsn)
		if err != nil {
			return fmt.Errorf("setup: open db: %w", err)
		}
		defer pool.Close()
		uc := setup.DBUserCreator{Q: gen.New(pool)}
		if err := commitSetup(ctx, a, paths, uc, os.Stdout); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "setup complete; restart the coordinator in normal mode")
		return nil

	case *interactive:
		a, err := promptAnswers()
		if err != nil {
			return err
		}
		// Generate secrets first so we can show and challenge the master key.
		s, err := setup.GenerateSecrets()
		if err != nil {
			return err
		}
		fp, err := setup.Fingerprint(s.MasterKeyHex)
		if err != nil {
			return err
		}
		fmt.Printf("MASTER KEY (back this up securely — it cannot be recovered):\n  %s\n  fingerprint: %s\n", s.MasterKeyHex, fp)
		// Require the operator to type back the fingerprint.
		scanner := bufio.NewScanner(os.Stdin)
		for {
			fmt.Print("Type the fingerprint to confirm you have saved the master key: ")
			if !scanner.Scan() {
				return errors.New("setup: stdin closed before fingerprint confirmation")
			}
			typed := strings.TrimSpace(scanner.Text())
			if typed == fp {
				break
			}
			fmt.Fprintln(os.Stderr, "Fingerprint mismatch; try again.")
		}
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return errors.New("setup: DATABASE_URL must be set")
		}
		pool, err := db.Open(ctx, dsn)
		if err != nil {
			return fmt.Errorf("setup: open db: %w", err)
		}
		defer pool.Close()
		uc := setup.DBUserCreator{Q: gen.New(pool)}
		if err := setup.Commit(ctx, a, paths, s, uc); err != nil {
			return err
		}
		fmt.Fprintln(os.Stdout, "setup complete; restart the coordinator in normal mode")
		return nil

	default:
		fmt.Fprintln(os.Stderr, "usage: novactl setup (--config-file <path> | --interactive)")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "  --config-file <path>   non-interactive: read answers from YAML file")
		fmt.Fprintln(os.Stderr, "  --interactive          prompt for each field on stdin")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Env vars: DATABASE_URL (required), NOVA_CONFIG_DIR (default /etc/nova),")
		fmt.Fprintln(os.Stderr, "          NOVA_SECRETS_DIR (default /run/secrets)")
		return errors.New("setup: specify --config-file or --interactive")
	}
}

// promptAnswers interactively reads each Answers field from os.Stdin.
func promptAnswers() (setup.Answers, error) {
	scanner := bufio.NewScanner(os.Stdin)
	prompt := func(label string) (string, error) {
		fmt.Printf("%s: ", label)
		if !scanner.Scan() {
			return "", fmt.Errorf("setup: stdin closed while reading %q", label)
		}
		return strings.TrimSpace(scanner.Text()), nil
	}
	promptBool := func(label string) (bool, error) {
		v, err := prompt(label + " [y/N]")
		if err != nil {
			return false, err
		}
		return strings.EqualFold(v, "y"), nil
	}
	// promptPassword reads a credential without echoing it (matching cmdLogin),
	// requiring a confirmation re-entry so a typo can't silently set the password.
	promptPassword := func(label string) (string, error) {
		for {
			fmt.Printf("%s: ", label)
			b1, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", fmt.Errorf("setup: read password: %w", err)
			}
			fmt.Print("Confirm password: ")
			b2, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", fmt.Errorf("setup: read password: %w", err)
			}
			if string(b1) != string(b2) {
				fmt.Fprintln(os.Stderr, "passwords do not match; try again")
				continue
			}
			return string(b1), nil
		}
	}

	var a setup.Answers
	var err error

	if a.Hostname, err = prompt("Hostname (e.g. img.example.com)"); err != nil {
		return a, err
	}
	if a.ContactEmail, err = prompt("Contact email"); err != nil {
		return a, err
	}
	if a.DisplayName, err = prompt("Display name (optional)"); err != nil {
		return a, err
	}
	if a.AdminEmail, err = prompt("Admin email"); err != nil {
		return a, err
	}
	if a.AdminPassword, err = promptPassword("Admin password (min 12 chars)"); err != nil {
		return a, err
	}
	if a.TLSMode, err = prompt("TLS mode (dev-self-signed|http-01|dns-01|static|onion)"); err != nil {
		return a, err
	}
	if a.TLSMode == "static" {
		if a.CertPath, err = prompt("Cert path"); err != nil {
			return a, err
		}
		if a.KeyPath, err = prompt("Key path"); err != nil {
			return a, err
		}
	}
	if a.AuthMode, err = prompt("Auth mode (local|external)"); err != nil {
		return a, err
	}
	if a.AuthMode == "external" {
		if a.IssuerURL, err = prompt("Issuer URL"); err != nil {
			return a, err
		}
		if a.ClientID, err = prompt("Client ID"); err != nil {
			return a, err
		}
	}
	if a.PublicUploads, err = promptBool("Allow public uploads"); err != nil {
		return a, err
	}
	if a.PublicUploads {
		if a.TosURL, err = prompt("Terms of service URL"); err != nil {
			return a, err
		}
	}
	if a.Paranoid, err = promptBool("Paranoid mode (suppress source-IP recording)"); err != nil {
		return a, err
	}

	if err := a.Validate(); err != nil {
		return setup.Answers{}, err
	}
	return a, nil
}

// --------------------------------------------------------------------------
// main
// --------------------------------------------------------------------------

func usage() {
	fmt.Fprintln(os.Stderr, "usage: novactl <auth|signed-url|moderation|keys|setup> <subcommand>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  auth login [--url <base>] [--username <u>]")
	fmt.Fprintln(os.Stderr, "  auth whoami")
	fmt.Fprintln(os.Stderr, "  auth logout")
	fmt.Fprintln(os.Stderr, "  signed-url sign --path <p> [--ttl <secs>] --aud <origin>")
	fmt.Fprintln(os.Stderr, "  moderation quarantine <cid> [--case <id>] [--reason <s>] [--tombstone-after 14d] [--legal-hold]")
	fmt.Fprintln(os.Stderr, "  moderation takedown <cid> [--case <id>] [--reason <s>] [--no-confirm]")
	fmt.Fprintln(os.Stderr, "  moderation clear-legal-hold <cid> [--case-id <ref>] [--reason <s>] [--no-confirm]")
	fmt.Fprintln(os.Stderr, "  moderation restore <cid> [--reason <s>]")
	fmt.Fprintln(os.Stderr, "  moderation list [--per-page <n>]")
	fmt.Fprintln(os.Stderr, "  keys rotate-master --from <label> --to <label> [--no-confirm]")
	fmt.Fprintln(os.Stderr, "  keys status")
	fmt.Fprintln(os.Stderr, "  setup --config-file <path>   (first-run, non-interactive)")
	fmt.Fprintln(os.Stderr, "  setup --interactive           (first-run, interactive prompts)")
}

func main() {
	args := os.Args[1:]
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}

	var err error
	switch args[0] {
	case "auth":
		err = runAuth(args[1:])
	case "signed-url":
		err = runSignedURL(args[1:])
	case "moderation":
		err = cmdModeration(args[1:])
	case "keys":
		err = cmdKeys(os.Args[2:])
	case "setup":
		err = cmdSetup(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "novactl: %v\n", err)
		os.Exit(1)
	}
}

func runAuth(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "login":
		return cmdLogin(args[1:])
	case "whoami":
		return cmdWhoami()
	case "logout":
		return cmdLogout()
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}

func runSignedURL(args []string) error {
	if len(args) < 1 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "sign":
		return cmdSignedURLSign(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", args[0])
		usage()
		os.Exit(2)
		return nil
	}
}
