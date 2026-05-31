// Package main is the Nova command-line client.
//
// Subcommands:
//
//	novactl auth login [--url <base>] [--username <u>]
//	novactl auth whoami
//	novactl auth logout
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/term"
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
		ae := readAPIError(resp.Body)
		if ae.Code == "invalid_credentials" || ae.Code != "" {
			fmt.Fprintln(os.Stderr, "invalid credentials")
		} else {
			fmt.Fprintln(os.Stderr, "invalid credentials")
		}
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
// main
// --------------------------------------------------------------------------

func usage() {
	fmt.Fprintln(os.Stderr, "usage: novactl auth <login|whoami|logout>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  auth login [--url <base>] [--username <u>]")
	fmt.Fprintln(os.Stderr, "  auth whoami")
	fmt.Fprintln(os.Stderr, "  auth logout")
}

func main() {
	args := os.Args[1:]
	if len(args) < 2 || args[0] != "auth" {
		usage()
		os.Exit(2)
	}

	sub := args[1]
	rest := args[2:]

	var err error
	switch sub {
	case "login":
		err = cmdLogin(rest)
	case "whoami":
		err = cmdWhoami()
	case "logout":
		err = cmdLogout()
	default:
		fmt.Fprintf(os.Stderr, "novactl: unknown subcommand %q\n\n", sub)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "novactl: %v\n", err)
		os.Exit(1)
	}
}
