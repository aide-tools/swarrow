# Configuration

Swarrow reads a single YAML document as its complete server and deployment policy. The implementation validates this initial schema strictly and rejects missing or unrecognised policy rather than inferring it. The schema may change before Swarrow's first stable release.

The [GitHub identity policy](design.md#github-identity-policy) explains the meaning and security rationale of the identity fields.

## Example

```yaml
version: 1

server:
  listen: 127.0.0.1:8080

github:
  audience: https://deploy.example.net

deployments:
  - name: example-web
    identity:
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

The `repository_id`, `workflow_ref` and `environment` values apply the exact matches required by the [GitHub identity policy](design.md#github-identity-policy). Here, `environment` is the GitHub Actions environment assigned to the job, not an operating-system variable or part of the Swarm service configuration. The readable `repository` value exists only to make diagnostics recognisable to an operator.

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

## Validation

Swarrow validates configuration before making it available as policy. It rejects the document when:

- The input is not exactly one YAML document or contains an unknown field
- It uses a YAML alias or merge key
- `version` is not `1`
- `server.listen` is not a `host:port` address with a numeric port between 1 and 65535
- `github.audience` is empty
- No deployments are defined
- A deployment omits its name, `repository_id`, `workflow_ref`, `environment`, service or image
- A `repository_id` is not the canonical positive decimal form of a GitHub repository ID
- Deployment names or concrete Swarm service targets are duplicated
- A target image is not a valid container image repository, or includes a tag or digest
- Configuration attempts to select another issuer or enable reusable workflows

The readable `repository` field is optional and used only for diagnostics. The same workflow identity may appear in several deployments when the operator deliberately grants it access to several targets.

Runtime policy mutation and configuration overlays are not part of the first version.

Replay handling, request idempotency and retry semantics are defined by the [deployment request lifecycle](design.md#deployment-request-lifecycle).
