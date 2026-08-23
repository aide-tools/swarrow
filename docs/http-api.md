# HTTP API

Swarrow exposes one deployment operation and one liveness check. The [deployment request lifecycle](design.md#deployment-request-lifecycle) defines why the operation is synchronous and how Swarrow applies, observes and retries an image update.

## Running the server

Start Swarrow with one explicit configuration file:

```sh
swarrow serve --config /etc/swarrow/config.yaml
```

The server validates the complete file and discovers GitHub's OpenID Connect provider before listening. It connects only to the local Docker socket at `/var/run/docker.sock`; Docker client environment variables do not redirect or add credentials to that connection.

Swarrow emits JSON operational and deployment audit logs to standard error. Deployment events record the requested deployment and digest, verified repository identity, workflow ref, GitHub Actions environment, HTTP status and established lifecycle result. They do not record the bearer token, its `jti`, Docker service identifiers, service specifications or raw Docker errors.

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
| `504 Gateway Timeout` | The configured request timeout expired before Swarrow could confirm that Docker selected the requested image |

Error responses contain a stable code and a generic message. When Swarrow established part of a deployment lifecycle before an error, the response may also contain its action and last observed state. Authentication and internal Docker errors are not returned verbatim.

## Network boundary

Swarrow serves plain HTTP and relies on an operator-managed reverse proxy for HTTPS. The proxy should restrict access as appropriate for the installation, enforce connection and request-rate limits and preserve the `Authorization` header. Its response timeout must exceed `server.request_timeout` only for the Swarrow route; it does not need to be changed globally.
