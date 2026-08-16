# Design

## Purpose

Swarrow connects two separately owned parts of a Docker Swarm deployment:

- Infrastructure configuration defines services, networking, storage, secrets, placement, resources and rollout behaviour
- Application repositories build images and decide when to release them

Swarrow carries a release from the application repository to an existing Swarm service without allowing the repository to control the rest of the service definition.

## Ownership model

| Concern | Owner |
| --- | --- |
| Service topology and runtime privileges | Infrastructure operator |
| Deployment policy | Infrastructure operator |
| Application source and image build | Application repository |
| Release timing and selected image digest | Authorised workflow |
| Current service image | Docker Swarm runtime |
| Authentication and constrained image update | Swarrow |

This intentionally separates infrastructure state from release state. Swarrow does not read application deployment configuration or apply a Compose file from an application repository.

## Proposed request flow

1. An authorised GitHub Actions workflow builds an image and pushes it to the configured registry.
2. The workflow requests a short-lived OpenID Connect token from GitHub.
3. It sends the token, a configured deployment name and an immutable digest to Swarrow over HTTPS.
4. Swarrow verifies the token issuer, audience, signature and time constraints.
5. Swarrow matches immutable identity claims to a locally configured deployment policy.
6. It constructs the image reference from the configured repository and supplied digest. The caller cannot supply a service name or image repository.
7. It inspects the current Swarm service, copies its specification and changes only the container image.
8. It updates the service using Docker's optimistic version index, observes the resulting rollout and reports its observed conclusion.

## Deployment request lifecycle

The initial API uses one synchronous request for each deployment. The GitHub Actions job waits while Swarrow applies at most one image change and observes the corresponding Swarm rollout. Swarrow then returns the state it observed and stops. The caller does not poll a separate status endpoint.

### Applying the image

Swarrow inspects the current service before deciding whether to update it. If the desired image differs, it copies the service specification, changes only the container image and submits the update with the inspected version index.

The update tells Docker to retain any registry credentials already stored with the current service. Swarrow does not accept registry credentials from the caller or read an operator's Docker CLI configuration. Because the caller supplies an immutable digest, Swarrow does not ask Docker to resolve a tag or pin the image through a registry lookup.

If the service already has the desired image, Swarrow does not submit another update. It observes an active rollout for that image if one exists, otherwise it reports that no change was required. This distinction keeps the action Swarrow took separate from the rollout state it observed.

### Observing the rollout

After applying or finding the desired image, Swarrow periodically inspects the service and its tasks. It follows that rollout until Swarm reports completion, pauses or fails the update, rolls it back, another update supersedes it or the observation timeout expires. Swarm remains responsible for update order, parallelism, health monitoring and automatic rollback according to the service specification owned by the infrastructure repository.

Swarrow accepts a service-and-task snapshot only when service inspections immediately before and after the task listing report the same service version. If the service changes between those calls, Swarrow discards the mixed snapshot and tries again.

Before Docker publishes a rollout start time, Swarrow records the stable service version with an unfinished observation. Docker may then publish the start time and advance the service version during the same observation; Swarrow accepts that expected transition and replaces the provisional version with the start time as the durable rollout identity. If an exact retry instead supplies a provisional version from an earlier observation, any version change or newly visible rollout status is conservatively reported as `superseded` because Swarrow cannot prove it still represents the original rollout. A durable rollout identity that changes or disappears is also `superseded`.

Task diagnostics require the rollout start time as a trustworthy lower time boundary. Swarrow still inspects active tasks to decide whether a statusless rollout has work remaining, but omits task details from the response until Docker publishes that boundary. A replacement rollout's start time becomes the upper boundary for diagnostics from the rollout that Swarrow was following.

If Docker exposes no rollout status, Swarrow lists active tasks and then re-inspects the service before concluding. It continues observing while any active task still targets another image, regardless of which caller submitted the update. If the service remains unchanged and no active task requires the image change, Swarrow reports the rollout as completed. This includes replicated services scaled to zero and global services without eligible tasks. Replicated and global jobs are rejected before mutation because they do not expose the rolling-update lifecycle Swarrow observes.

The response reports Swarrow's action, such as `updated` or `no_change`, separately from the observed conclusion. The conclusions distinguish `completed`, `failed`, `rolled_back`, `superseded` and `in_progress`. The [HTTP API](http-api.md) defines their response shape and status codes.

An update submission may become indeterminate if Swarrow sends it to Docker but loses the response through a timeout, cancellation or transport failure. Swarrow must not infer from the missing response that Docker rejected the update. It reports the action as `indeterminate` and does not submit another update during that request. If Docker definitively refuses the update, Swarrow instead reports it as `rejected`. If time remains after an indeterminate submission, Swarrow re-inspects the service to establish whether Docker accepted the change.

One required server-wide timeout bounds the complete request from the moment Swarrow receives it, including authentication, queueing, applying and rollout observation. Five minutes is the intended initial value. If the timeout expires before Swarrow attempts an update, it reports that no update was applied. If an accepted rollout remains active when the timeout expires, Swarrow reports `in_progress` and stops observing; it does not cancel the rollout, which continues in Swarm. A cancelled client request also stops observation without reversing an update already accepted by Swarm. Any reverse proxy must allow the request to remain open for at least the configured request timeout.

Swarrow does not stream progress, monitor the rollout after the request ends, initiate a rollback or retain deployment history. Adding asynchronous operations, status endpoints or continuous reconciliation would require a separate design.

### Replay and retries

Every accepted identity token must contain a GitHub-generated `jti` claim. The first request using that token binds the `jti` to the exact deployment and digest. An exact repeat is an idempotent retry: it resumes observation or returns the outcome already recorded without repeating an accepted service update. If observation failed before reaching a conclusion, the retry continues from any rollout identity already established. If submission was indeterminate, the retry first inspects the service image and version. It may submit the update only when that inspection establishes that Docker did not accept the earlier attempt; otherwise it observes the accepted update or returns `indeterminate` without another mutation. Reusing the same `jti` with another deployment or digest is rejected.

Swarrow keeps used `jti` records in a fixed-capacity memory cache until their tokens can no longer pass time validation, including the verifier's fixed expiry clock-skew allowance. If the cache has no capacity for another record, Swarrow rejects the request instead of evicting a record for a token that may still be accepted and reopening a replay window.

The initial process retains at most 1,024 unexpired replay records. This is an internal safety limit rather than a configuration overlay.

Because those records do not survive a restart, Swarrow establishes a restart cutoff before accepting tokens. The cutoff is the first whole second after the process start time plus the verifier's fixed 30-second allowance for a token issued slightly in the future because of clock skew. Swarrow rejects tokens whose `iat` is earlier than that cutoff. This deliberately creates a short period after startup when deployments are rejected; a workflow must obtain new tokens and retry until a token's `iat` reaches the cutoff. The exact delay depends on the difference between GitHub's clock and the server's clock.

A fresh token accepted after restart is a new authorisation. Inspecting the service avoids another update when the requested image remains its current target. If the earlier rollout was rolled back or superseded while Swarrow was unavailable, no in-memory evidence survives to distinguish that history and the fresh request may apply the digest again. Preventing that would require durable operation history. This model assumes one Swarrow process in the initial version.

### Concurrent requests

Swarrow creates one fixed-capacity worker queue for each concrete Swarm service in the validated configuration. Requests for a service are processed in the order they enter its queue, one complete apply-and-observe lifecycle at a time. Requests for different services may proceed concurrently.

Each initial worker admits at most 16 waiting requests in addition to the request it is processing. This is an internal safety limit rather than a per-service setting.

A request retains its original timeout while queued. If that timeout expires before processing begins, Swarrow removes the request without calling Docker and reports that no update was applied. A full queue is also rejected without calling Docker. Docker's version index still protects against changes made outside Swarrow, which must be reported explicitly rather than overwritten. Application workflows remain responsible for deciding release order.

## GitHub identity policy

GitHub Actions jobs can request a short-lived OpenID Connect token containing signed claims about the running job. Swarrow first authenticates that token as a statement from GitHub, then authorises the job by comparing a small set of its claims with local deployment policy.

Authentication discovers GitHub's signing keys through its fixed issuer, accepts only RS256 signatures and requires the configured audience to be the token's only audience. It applies one non-configurable 30-second clock-skew allowance to the `exp`, `nbf` and `iat` constraints. Together, authentication and authorisation evaluate these identity claims:

| Claim | Meaning | Requirement |
| --- | --- | --- |
| `iss` | The identity provider that created the token | Fixed to GitHub.com's canonical `https://token.actions.githubusercontent.com` issuer |
| `aud` | The intended recipient of the token | Identifies only the configured Swarrow audience |
| `iat` | The whole second when GitHub issued the token | Present and at or after the current process's restart cutoff |
| `jti` | GitHub's unique identifier for this token | Present and unused for any different deployment request |
| `repository_id` | GitHub's stable numeric identity for the application repository | Exactly matches the configured repository ID |
| `workflow_ref` | The caller workflow file and Git ref | Exactly matches the configured workflow path and ref |
| `environment` | The GitHub environment assigned to the job | Exactly matches the configured environment name |
| `job_workflow_ref` | The second workflow that defines a job delegated to a reusable workflow | Absent |

The `repository_id`, `workflow_ref` and `environment` constraints are mandatory and non-empty for every deployment.

A direct workflow defines the deployment job in the configured workflow file. A reusable workflow instead delegates that job to a second workflow, which GitHub identifies through `job_workflow_ref` while retaining information about the caller. Supporting both identities requires an explicit policy for the caller and called workflow. The initial version avoids that ambiguity by accepting direct workflows only and rejecting any token containing `job_workflow_ref`.

### Why other claims are not used

- `repository` is a readable name that can change or be reused. It may appear in diagnostics, but it cannot grant authority or replace `repository_id`.
- `sub` is a composite subject whose default format can differ between GitHub repositories. Swarrow uses the signed individual claims instead of parsing it.
- `ref` is not checked separately because `workflow_ref` already identifies the authorised direct workflow file and Git ref.
- `event_name` is not constrained. The authorised workflow owns its trigger rules, so any trigger that reaches its deployment job and passes its environment protections may deploy.
- The initiating actor is not an authority. Repository workflow controls and environment protections form the trust boundary instead of a mutable person or bot identity.

## Core invariants

The implementation must preserve these properties:

1. A caller cannot select a Docker service directly.
2. A caller cannot select or override an image repository directly.
3. Every accepted image is identified by an OCI digest, not only a mutable tag.
4. Authorisation uses immutable repository identity in addition to readable names.
5. The submitted workflow identity must match the configured immutable repository ID, direct workflow ref and environment.
6. A service update changes only its container image. All other fields are copied from the inspected service specification.
7. Concurrent changes are not overwritten silently. A stale service version must fail and be inspected again.
8. Credentials and complete identity tokens are never written to logs.
9. Denied requests do not reach the Docker API.
10. No endpoint exposes a generic Docker operation.

## Initial scope

The first useful version is expected to provide:

- GitHub Actions OpenID Connect authentication
- Direct GitHub Actions workflow identity
- File-based deployment policy
- One fixed repository and service per deployment policy
- Digest-only image updates
- Request idempotency and per-service serialisation
- Structured audit events
- Deployment result reporting
- A health endpoint and configuration validation

## Non-goals

Swarrow is not intended to:

- Build or push container images
- Interpret or deploy Compose files
- Create stacks or services
- Manage secrets, configs, networks, mounts or registry credentials for callers
- Change replicas, placement, commands, environment or resource limits
- Poll mutable image tags
- Provide a general Docker API proxy
- Provide a general multi-tenant Docker control plane
- Replace workload isolation or container hardening
- Run deployment jobs through reusable workflows

## Interaction with stack deployment

A Swarm stack file must contain an image even when release ownership belongs to the application repository. Running `docker stack deploy` later may therefore replace the live image with the value in that file.

Swarrow does not own or continuously reconcile stack topology. Operators should preserve each existing service image when applying topology changes, for example by resolving the current image into a required stack variable before deployment. An initial service creation must be given an explicit bootstrap digest.

This behaviour must be documented clearly, but automation for it is outside the first Swarrow release.

## Privilege model

Swarrow must communicate with a Swarm manager and is therefore a privileged component. Its security depends on exposing a much smaller interface than the Docker API and enforcing policy before any Docker operation.

The service should run with an otherwise restricted host identity and should not receive unrelated host credentials. Deployment behind TLS is required because a valid workflow token authorises a privileged operation during its short lifetime.

## References

- [GitHub OpenID Connect reference](https://docs.github.com/en/actions/reference/security/oidc)
- [Using OpenID Connect with reusable workflows](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-with-reusable-workflows)
- [Go OpenID Connect client](https://github.com/coreos/go-oidc)
- [Docker Swarm rolling updates](https://docs.docker.com/engine/swarm/swarm-tutorial/rolling-update/)
- [Docker Swarm service update behaviour](https://docs.docker.com/engine/swarm/services/#configure-a-services-update-behavior)
