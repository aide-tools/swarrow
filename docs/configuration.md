# Configuration

Swarrow reads a single YAML document as its complete server and deployment policy. It validates the schema strictly and rejects missing or unrecognised policy rather than inferring it.

The [GitHub identity policy](design.md#github-identity-policy) explains the meaning and security rationale of the identity fields.

## Example

```yaml
version: 1

server:
  listen: 127.0.0.1:8080
  request_timeout: 5m

github:
  audience: https://deploy.example.net
  job_workflow_ref: example/swarrow-deploy/.github/workflows/deploy.yml@refs/tags/v1

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

The `repository_id`, `workflow_ref`, `job_workflow_ref` and `environment` values apply the exact matches required by the [GitHub identity policy](design.md#github-identity-policy). Here, `workflow_ref` identifies the application workflow that called the shared deployment workflow. The top-level `github.job_workflow_ref` identifies that shared workflow and applies to every deployment unless an identity supplies its own `job_workflow_ref`. The configured value must match GitHub's complete claim, including its Git ref.

When neither location configures `job_workflow_ref`, Swarrow accepts only a job defined directly in the application workflow. This preserves the direct-workflow policy used by configurations written before reusable workflow support. The `environment` is the GitHub Actions environment assigned to the job, not an operating-system variable or part of the Swarm service configuration. The readable `repository` value exists only to make diagnostics recognisable to an operator.

The required `request_timeout` bounds the complete request from receipt through authorisation, queueing, Docker mutation and rollout observation. Five minutes is the intended initial value. A reverse proxy in front of Swarrow must permit a request to remain open for at least this duration.

The caller does not submit the `service` or `image` values. A request identifies the configured deployment and supplies only an immutable digest:

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

The configured `target.image` is only a repository name and must not contain a tag or digest. Each deployment request must instead supply a canonical SHA-256 digest in the form `sha256:` followed by 64 lowercase hexadecimal characters. Tags such as `example-123abc`, semantic versions and `latest` are not accepted as release identifiers; a tag remains mutable even when its name is derived from a commit. Swarrow combines the configured repository with the supplied digest and does not ask Docker to resolve a tag through the registry.

The [HTTP API](http-api.md) defines request validation, responses and status codes.

## Validation

Swarrow validates configuration before making it available as policy. It rejects the document when:

- The input is not exactly one YAML document or contains an unknown field
- It uses a YAML alias or merge key
- `version` is not `1`
- `server.listen` is not a `host:port` address with a numeric port between 1 and 65535
- `server.request_timeout` is not a positive Go-style duration such as `5m`
- `github.audience` is empty
- An optional `github.job_workflow_ref` or deployment `identity.job_workflow_ref` has leading or trailing whitespace
- No deployments are defined
- A deployment omits its name, `repository_id`, `workflow_ref`, `environment`, service or image
- A `repository_id` is not the canonical positive decimal form of a GitHub repository ID
- Deployment names or concrete Swarm service targets are duplicated
- A target image is not a valid container image repository, or includes a tag or digest
- Configuration attempts to select another issuer

The readable `repository` field is optional and used only for diagnostics. A deployment-level `job_workflow_ref` is also optional and overrides the top-level default when the operator deliberately authorises another shared workflow for that deployment. The same workflow identity may appear in several deployments when the operator deliberately grants it access to several targets.

Runtime policy mutation and configuration overlays are not part of the first version.

Replay handling, request idempotency and retry semantics are defined by the [deployment request lifecycle](design.md#deployment-request-lifecycle).
