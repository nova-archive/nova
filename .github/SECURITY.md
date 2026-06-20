# Security Policy

## Reporting a Vulnerability

Please report security vulnerabilities by opening a private security
advisory on this repository's GitHub Security tab.

Do **not** report vulnerabilities via public issues, pull requests, or
discussion threads.

We aim to acknowledge reports within 72 hours and to publish a fix or
mitigation within 14 days for critical issues.

## Supported Versions

Nova is pre-1.0 and under active development. Releases are cut per
milestone under unique semantic versions (see
[`docs/VERSIONING.md`](../docs/VERSIONING.md)); the latest tagged
milestone on `main` is the only supported line until a 1.0 release
establishes maintained release branches. Security fixes land on `main`
and in the next milestone tag.

## Threat Model

See [`docs/THREAT_MODEL.md`](../docs/THREAT_MODEL.md). The threat model
documents what Nova is designed to defend against and what is explicitly
out of scope — including the donor-blind (not operator-blind) trust
boundary, malicious donors, a compromised coordinator, hostile crawlers,
network observers, and supply-chain attackers.
