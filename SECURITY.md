# Security Policy

## Reporting a vulnerability

Please use GitHub's private security advisories (Security → Advisories →
Report) rather than opening a public issue. Include reproduction details and
the affected commit.

## Scope

This controller holds kubeconfig credentials for every workload cluster it
watches and can delete Longhorn replicas and pods. Reports about RBAC scope,
credential handling, or destructive-path logic are especially welcome.

## Supported versions

Only the latest `main` (0.x-alpha stage) is supported.
