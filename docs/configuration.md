# Illustrative configuration

This document makes the proposed policy concrete enough to review. It is not a stable schema and is not yet accepted by an implementation.

## Example

```yaml
version: 1

server:
  listen: 127.0.0.1:8080

identity_providers:
  github:
    issuer: https://token.actions.githubusercontent.com
    audience: https://deploy.example.net

deployments:
  - name: example-web
    identity:
      provider: github
      repository_id: "123456789"
      repository: example/example-web
      environment: production
      workflow_ref: example/example-web/.github/workflows/deploy.yml@refs/heads/main
    target:
      service: example_web
      image: ghcr.io/example/example-web
```

All names and identifiers are fictional.

## Intended meaning

The `example-web` deployment grants one capability:

> The configured GitHub repository, running the configured workflow on the configured environment, may update `example_web` to an immutable digest from `ghcr.io/example/example-web`.

The readable `repository` value exists for diagnostics. The immutable `repository_id` is the primary repository identity.

The caller does not submit the `service` or `image` values. A request is expected to identify the configured deployment and supply only an immutable digest:

```http
POST /v1/deployments/example-web
Authorization: Bearer <short-lived GitHub OIDC token>
Content-Type: application/json

{
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

Swarrow would construct this final image reference:

```text
ghcr.io/example/example-web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

## Policy defaults

The eventual schema should fail closed:

- Unknown configuration fields are errors
- Missing identity constraints are not inferred from readable names
- Tags are rejected where a digest is required
- Duplicate deployment names, services or identity mappings are errors unless a deliberate sharing model is designed later
- Configuration is validated before the server begins accepting requests
- Runtime policy mutation is not part of the first version

Whether every deployment must constrain an environment, workflow ref and source ref remains a design decision. The implementation should make omitted constraints explicit rather than treating an empty value ambiguously.
