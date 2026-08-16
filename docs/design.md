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
8. It updates the service using Docker's optimistic version index and reports the deployment result.

## GitHub identity policy

The initial version accepts GitHub.com OpenID Connect tokens only for jobs defined directly in the authorised application repository. Reusable workflows are not supported. A token containing a `job_workflow_ref` claim must be rejected rather than interpreted as a direct-workflow identity.

Authentication must verify the token signature, the canonical `https://token.actions.githubusercontent.com` issuer, the one configured audience and the `exp`, `nbf` and `iat` time constraints. The token must identify only the configured Swarrow audience; accepting an additional or fallback audience would broaden its authority.

After authentication, a deployment policy must require exact matches for these claims:

| Claim | Requirement |
| --- | --- |
| `repository_id` | Matches the configured immutable numeric repository ID |
| `workflow_ref` | Matches the configured workflow path and ref |
| `environment` | Matches the configured GitHub environment name |

All three constraints are mandatory and non-empty. The readable `repository` claim may be retained for diagnostics, but it cannot grant authority or replace `repository_id`.

Swarrow does not parse or authorise from the `sub` claim. GitHub repositories may use different default subject formats, while the signed identity claims above provide the values Swarrow needs directly. The `workflow_ref` already constrains the direct workflow file and ref, so the initial policy does not add separate `ref`, `event_name` or actor constraints.

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
