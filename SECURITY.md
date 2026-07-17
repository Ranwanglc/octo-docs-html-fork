# Security Policy

## Supported versions

octo-doc is pre-1.0; only the **latest release** receives security fixes. Please
upgrade to the most recent [release](https://github.com/Mininglamp-OSS/octo-docs-html/releases)
before reporting.

| Version | Supported |
| --- | --- |
| latest release | ✅ |
| older releases | ❌ |

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately through GitHub's
[**Report a vulnerability**](https://github.com/Mininglamp-OSS/octo-docs-html/security/advisories/new)
form (Security → Advisories). This opens a private advisory visible only to the
maintainers.

Please include, where possible:

- the affected version or commit,
- a description of the issue and its impact,
- steps to reproduce (a minimal proof-of-concept helps), and
- any suggested remediation.

## What to expect

- **Acknowledgement** within 3 business days.
- An initial assessment and severity triage within 7 business days.
- Coordinated disclosure: we'll work with you on a fix and a release, and credit
  you in the advisory unless you prefer otherwise.

## Scope

In scope: the server (`cmd/octo-doc`) and the overlay served to browsers. Out of
scope: vulnerabilities in third-party
dependencies (report those upstream), and issues that require a
misconfigured deployment contrary to [docs/SELF_HOSTING.md](docs/SELF_HOSTING.md)
(e.g. `COOKIE_SECURE=false` in production, an exposed `WRITE_TOKEN`).

Hardening guidance for operators is in the production checklist in
[docs/SELF_HOSTING.md](docs/SELF_HOSTING.md); the access-control model is
documented in [docs/AUTH.md](docs/AUTH.md).
