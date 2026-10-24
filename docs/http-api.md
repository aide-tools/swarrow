# HTTP API

Swarrow exposes one deployment operation and one liveness check. The [deployment request lifecycle](design.md#deployment-request-lifecycle) defines why the operation is synchronous and how Swarrow applies, observes and retries an image update.

## Running the server

Start Swarrow with one explicit configuration file:

```sh
swarrow serve --config /etc/swarrow/config.yaml
```

The server validates the complete file and discovers GitHub's OpenID Connect provider before listening. It connects only to the local Docker socket at `/var/run/docker.sock`; Docker client environment variables do not redirect or add credentials to that connection.

Swarrow emits JSON operational and deployment audit logs to standard error. Deployment events record the requested deployment and digest, verified repository identity, workflow ref, GitHub Actions environment, HTTP status and established lifecycle result. They do not record the bearer token, its `jti`, Docker service identifiers, service specifications or raw Docker errors.

After a restart, Swarrow logs a warning with the conservative time when freshly issued GitHub tokens are guaranteed to pass its replay-protection cutoff. The HTTP listener and `/healthz` remain available during this warm-up period, and Swarrow logs again when deployment authentication reaches that ready time.

## Health

`GET /healthz` returns `200 OK` while the HTTP process is available:

```json
{"status":"ok"}
```

`HEAD /healthz` returns the same status without a body. This is a liveness check; it does not contact GitHub or Docker.

## Deploying an image

Send one canonical SHA-256 digest to a configured deployment:

```http
POST /v1/deployments/example-web
Authorization: Bearer <short-lived GitHub OIDC token>
Content-Type: application/json

{
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
}
```

The request must contain exactly one JSON object and no field other than `digest`. Its body is limited to 1 KiB. The deployment name selects local policy; the caller cannot provide a Docker service or image repository.

The request remains open while Swarrow authenticates, queues, applies and observes the deployment. There is no progress stream or separate status endpoint. A reverse proxy must permit the request to remain open for longer than `server.request_timeout`, with enough margin for Swarrow to write the final response.

If the timeout expires after Swarrow has confirmed that Docker selected the requested image, the response is `202 Accepted` with an `in_progress` conclusion. The rollout continues in Swarm after Swarrow stops watching it. A timeout before Swarrow can confirm the selected image instead returns `504 Gateway Timeout`.

A completed response has this shape:

```json
{
  "deployment": "example-web",
  "action": "updated",
  "conclusion": "completed",
  "image": "ghcr.io/example/example-web@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "message": "update completed",
  "tasks": {
    "running": 2,
    "pending": 0,
    "failed": 0
  }
}
```

`action` records what Swarrow established about the image mutation. `conclusion` records the rollout state it observed. Failed task details may be included when Swarm reports them. Docker service IDs and version indexes are never returned.

## Proposed job API

> [!NOTE]
> This section proposes job support for a later implementation. The current release does not support job targets or the preparation endpoint.

For a target configured with `mode: job`, first send `POST /v1/deployments/example-migrate/prepare` with the same Bearer authentication and `{ "digest": "sha256:..." }` body. This operation is read-only and uses the normal authorisation policy. A successful response is `200 OK` with `{ "execution": "<reference>" }`, where the reference is an opaque string of 64 lowercase hexadecimal characters. Preparation for an ordinary service target returns `400` with `invalid_execution`.

Retain that reference before submitting the job:

```http
POST /v1/deployments/example-migrate
Authorization: Bearer <short-lived GitHub OIDC token>
Content-Type: application/json

{
  "digest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "execution": "<reference returned by preparation>"
}
```

Submission uses the normal deployment response shape. `200` with `completed` means the job's task exited successfully. `502` with `failed` means the execution failed; it must not be followed by web deployment. `202` with `in_progress` means the image was selected but completion was still unknown when observation ended. `504` means Swarrow could not confirm that Docker selected the image before the request timeout. A disconnected response is also an unknown outcome. Repeat submission with the same digest and reference to resume observation, obtaining a fresh token when necessary. Do not prepare again to recover an uncertain submission.

A reference invalidated by another update returns `409` with a `superseded` conclusion; `job_busy` also returns `409` when the previous execution is active or lacks terminal evidence. A missing reference on a job target, a reference on an ordinary target or a reference supplied to preparation returns `400` with `invalid_execution`. Duplicate, unknown or malformed fields are rejected within the existing 1 KiB body limit. The reference is not a credential and every request is independently authenticated and authorised.

Preparing the current image returns its existing tracked execution; it does not rerun a failed job. A current image with no trustworthy execution record is rejected. Job definitions and completed tasks remain in Swarm; Swarrow does not clean them up. See the [job lifecycle](design.md#proposed-predefined-job-executions) for the evidence required to start and observe an execution.

## Status codes

| Status | Meaning |
| --- | --- |
| `200 OK` | The requested image is selected and its rollout completed, whether Swarrow updated it or found it already selected |
| `202 Accepted` | Swarrow established the requested image but the rollout remained in progress when observation ended |
| `400 Bad Request` | The JSON body or digest is invalid |
| `401 Unauthorized` | Bearer authentication is missing or invalid |
| `403 Forbidden` | The verified workflow is not authorised, or the token was reused for a different request |
| `408 Request Timeout` | The caller cancelled the request before Swarrow concluded it |
| `409 Conflict` | The rollout was superseded, or the service changed concurrently before Swarrow could update it |
| `429 Too Many Requests` | A bounded replay cache or service queue has no capacity for the request |
| `502 Bad Gateway` | Swarm reported a failed or rolled-back rollout, or Docker could not complete the operation |
| `503 Service Unavailable` | An otherwise valid token was issued too early after this Swarrow process started |
| `504 Gateway Timeout` | The configured request timeout expired before Swarrow could confirm that Docker selected the requested image |

The restart warm-up response uses the stable code `authentication_warming_up` and includes `Retry-After` in whole seconds. The workflow must wait, obtain a new GitHub OIDC token and then retry; resending the same token cannot change its signed issue time. Other authentication failures remain `401 Unauthorized`, and a token below the restart cutoff is no longer presented as retryable after the conservative ready time.

Other error responses contain a stable code and a generic message. When Swarrow established part of a deployment lifecycle before an error, the response may also contain its action and last observed state. Authentication and internal Docker errors are not returned verbatim.

## Network boundary

Swarrow serves plain HTTP and relies on an operator-managed reverse proxy for HTTPS. The proxy should restrict access as appropriate for the installation, enforce connection and request-rate limits and preserve the `Authorization` header. Its response timeout must exceed `server.request_timeout` only for the Swarrow route; it does not need to be changed globally.
