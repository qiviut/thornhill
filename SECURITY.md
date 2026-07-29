# Security policy

## Reporting a vulnerability

Please report suspected vulnerabilities through [GitHub private vulnerability reporting](../../security/advisories/new). Do not include exploit details, credentials, private infrastructure identifiers, or personal data in a public issue.

Include the affected revision, deployment model, reproduction conditions, impact, and the smallest safe proof needed to validate the report. Reports concerning approval boundaries, command execution, provider authentication, browser origin checks, or durable job recovery are especially useful.

## Supported version

Security fixes target the latest revision of `main`. Thornhill has not yet published a stable release series with backport guarantees.

## Deployment scope and threat model

Read [`docs/security-model.md`](docs/security-model.md) before assessing exposure,
identity, hooks, approvals, dependency promotion, retained data, or backup claims.
The supported deployment is one operator behind an operator-only Tailnet grant;
the gateway has no application-level user authentication, and browser `Origin`
checks are not identity. Findings that require shared or public reachability remain
useful, but should identify which supported deployment assumption is broken.

## Scope and safe testing

Use systems and accounts you own or are explicitly authorized to test. Do not test against third-party deployments, attempt to access another user's data, or submit live credentials. Test fixtures and pull-request CI must remain secretless and must not invoke production model or Hermes endpoints.
