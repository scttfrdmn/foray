# Security Policy

## Posture

foray's security model is structural (see [ARCHITECTURE.md §8](ARCHITECTURE.md)):

- **No untrusted-code sandbox.** Single-tenant self-install: you run your own
  intervention code on your own ephemeral GPU in your own AWS account. NDIF's
  hardest problem — isolating strangers on shared GPUs — is not present here. It
  returns only if a shared hosted tier is ever offered (deferred, not built).
- **No automatic egress of activations.** Saved values land in S3 in-region;
  only rendered pixels reach the browser. **Export is the deliberate
  exception:** a user can download their own saved values via a presigned,
  time-limited URL when they choose to — opt-in and user-initiated, governed by
  a Cedar `export` action an org can disable.
- **Cedar governs the agent.** Model sources, instance tiers, and budgets are
  policy, not code (`internal/brain/policy/foray.cedar`). The human-in-the-loop
  "Go" is the final gate; the brain never settles its own acceptance.

## Supported versions

The project is pre-1.0.0. Security fixes target the latest `main` and the most
recent tagged release.

## Reporting a vulnerability

Please report suspected vulnerabilities privately via
[GitHub Security Advisories](https://github.com/scttfrdmn/foray/security/advisories/new)
rather than a public issue. Include reproduction steps and impact. You will
receive an acknowledgement; please allow reasonable time for a fix before any
public disclosure.

Do **not** include live AWS credentials, account IDs, or customer data in a
report.
