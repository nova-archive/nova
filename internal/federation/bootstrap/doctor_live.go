package bootstrap

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// LiveOpts configures Plane B.
type LiveOpts struct {
	// ReadyzURL is the coordinator's loopback diagnostic endpoint. Reachable
	// because nova-doctor shares the coordinator's network namespace.
	ReadyzURL string

	// FederationAddr is the overlay address to probe with mTLS.
	FederationAddr string

	// Client identity for the mTLS probe. NEVER the CA key — nova-doctor does
	// not mount nova-fedpki.
	CAPEM, ClientCertPEM, ClientKeyPEM []byte

	Timeout time.Duration
}

// readyzResponse mirrors the coordinator's /readyz body.
type readyzResponse struct {
	Federation struct {
		Ready      bool   `json:"ready"`
		Iface      string `json:"iface"`
		BoundAddr  string `json:"bound_addr"`
		WaitingFor string `json:"waiting_for"`
		Since      string `json:"since"`
	} `json:"federation"`
}

// DoctorLive is Plane B of `federation doctor` (D-M7.2-3), run inside
// nova-doctor, which shares the coordinator's network namespace.
//
// net.iface, net.bind and net.ready are answered by the COORDINATOR via its
// own /readyz rather than probed from outside. The coordinator is the process
// that knows whether it bound and to what; asking it is both simpler and more
// authoritative than inferring it. This is also what keeps nova-admin away
// from the Docker socket — see D-M7.2-3.
func DoctorLive(ctx context.Context, opts LiveOpts) []Check {
	const plane = "B"
	var out []Check

	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	rz, rzErr := fetchReadyz(ctx, opts.ReadyzURL, timeout)

	// net.iface — the overlay interface exists and the coordinator sees it.
	switch {
	case rzErr != nil:
		out = append(out, Fail(plane, "net.iface",
			fmt.Sprintf("cannot reach the coordinator diagnostic endpoint: %v", rzErr)))
	case rz.Federation.Iface == "":
		out = append(out, Fail(plane, "net.iface", "coordinator reports no overlay interface configured"))
	case !rz.Federation.Ready && rz.Federation.WaitingFor != "":
		out = append(out, Fail(plane, "net.iface",
			fmt.Sprintf("coordinator is still waiting for interface %q", rz.Federation.WaitingFor)))
	default:
		out = append(out, Pass(plane, "net.iface",
			fmt.Sprintf("coordinator sees overlay interface %s", rz.Federation.Iface)))
	}

	// net.bind — the listener must be overlay-only. A wildcard bind exposes the
	// federation API off the overlay, which the whole trust model assumes away.
	switch {
	case rzErr != nil:
		out = append(out, Skip(plane, "net.bind", "coordinator diagnostic endpoint unreachable"))
	case rz.Federation.BoundAddr == "":
		out = append(out, Fail(plane, "net.bind", "federation listener is not bound"))
	default:
		host, _, err := net.SplitHostPort(rz.Federation.BoundAddr)
		switch {
		case err != nil:
			out = append(out, Fail(plane, "net.bind",
				fmt.Sprintf("bound address %q is not host:port", rz.Federation.BoundAddr)))
		case host == "0.0.0.0" || host == "::" || host == "":
			out = append(out, Fail(plane, "net.bind",
				fmt.Sprintf("federation listener is bound to %s — it must be overlay-only", rz.Federation.BoundAddr)))
		default:
			out = append(out, Pass(plane, "net.bind",
				fmt.Sprintf("federation listener bound overlay-only on %s", rz.Federation.BoundAddr)))
		}
	}

	// net.ready — the coordinator's own readiness verdict.
	switch {
	case rzErr != nil:
		out = append(out, Fail(plane, "net.ready", fmt.Sprintf("unreachable: %v", rzErr)))
	case !rz.Federation.Ready:
		detail := "coordinator reports federation not ready"
		if rz.Federation.WaitingFor != "" {
			detail += fmt.Sprintf(" (waiting for %s)", rz.Federation.WaitingFor)
		}
		out = append(out, Fail(plane, "net.ready", detail))
	default:
		out = append(out, Pass(plane, "net.ready", "coordinator reports federation ready"))
	}

	// net.mtls — a real handshake with the coordinator client identity.
	out = append(out, probeMTLS(ctx, plane, opts, timeout))

	return out
}

func fetchReadyz(ctx context.Context, url string, timeout time.Duration) (readyzResponse, error) {
	var rz readyzResponse
	if url == "" {
		return rz, fmt.Errorf("no readyz URL configured")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return rz, err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return rz, err
	}
	defer resp.Body.Close()

	// 503 is expected while degraded and still carries a readable body — the
	// reason for not-ready is exactly what an operator needs here.
	if err := json.NewDecoder(resp.Body).Decode(&rz); err != nil {
		return rz, fmt.Errorf("readyz body is not JSON: %w", err)
	}
	return rz, nil
}

func probeMTLS(ctx context.Context, plane string, opts LiveOpts, timeout time.Duration) Check {
	const id = "net.mtls"
	if opts.FederationAddr == "" {
		return Skip(plane, id, "no federation address configured")
	}
	if len(opts.ClientCertPEM) == 0 || len(opts.ClientKeyPEM) == 0 {
		return Skip(plane, id, "no coordinator client identity available to probe with")
	}
	cert, err := tls.X509KeyPair(opts.ClientCertPEM, opts.ClientKeyPEM)
	if err != nil {
		return Fail(plane, id, fmt.Sprintf("client identity does not load: %v", err))
	}
	pool, err := certPoolFromPEM(opts.CAPEM)
	if err != nil {
		return Fail(plane, id, fmt.Sprintf("federation CA does not load: %v", err))
	}

	d := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: timeout},
		Config: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS13,
			// The coordinator's serving certificate carries overlay IP and
			// hostname SANs; verification is against the federation CA.
			InsecureSkipVerify: false,
			ServerName:         hostOf(opts.FederationAddr),
		},
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := d.DialContext(ctx, "tcp", opts.FederationAddr)
	if err != nil {
		return Fail(plane, id, fmt.Sprintf("mTLS probe to %s failed: %v", opts.FederationAddr, err))
	}
	_ = conn.Close()
	return Pass(plane, id, fmt.Sprintf("mTLS handshake with %s succeeded", opts.FederationAddr))
}

func hostOf(hostPort string) string {
	h, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return strings.TrimSpace(hostPort)
	}
	return h
}
