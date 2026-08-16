# Threat model

## Security objective

Swarrow allows an authenticated continuous-integration workflow to update one approved Docker Swarm service to one approved image digest without granting that workflow general host, manager or Docker API access.

The objective is least-privilege delegation of a service image update. It is not isolation between an application and the runtime resources already assigned to that service.

## Assets

Swarrow is expected to protect:

- Docker Swarm manager authority
- Services outside the caller's deployment policy
- Non-image fields of the targeted service specification
- Deployment policy and operator configuration
- Workflow identity tokens while they are being processed
- Audit integrity and the accuracy of deployment results

## Actors and trust

### Infrastructure operator

The operator controls the host, Swarm manager and Swarrow policy. This actor is trusted to assign deployment capabilities correctly.

### Swarrow

Swarrow is trusted with privileged Docker access. A compromise of the process or its host can compromise the swarm. Its public interface and dependency set must therefore remain deliberately small.

### Identity provider

GitHub.com's canonical OpenID Connect issuer is trusted to sign accurate workflow identity claims. Swarrow must verify those claims using the issuer's published keys and must fail closed when verification cannot be completed.

### Application repository and workflow

An authorised workflow is trusted only to select a digest for its configured image and service. It is not trusted with another deployment target or with other Docker operations.

### Container image

The selected image is untrusted outside the privileges already granted to the target service. It may contain arbitrary or malicious application code.

## Trust boundaries

1. The public network boundary between a workflow and Swarrow
2. The identity boundary between token claims and a deployment policy
3. The policy boundary between caller-controlled input and operator-controlled service and image values
4. The privileged boundary between Swarrow and the Docker Engine API
5. The workload boundary between a deployed image and its existing runtime permissions

## Threats and required controls

### Forged or stolen identity

An attacker may forge a token, substitute token metadata or replay a captured request.

Required controls include signature and issuer verification, an exact audience, time validation, short token lifetimes, TLS, bounded request bodies and replay or idempotency handling. Tokens must never appear in logs or error responses.

### Repository or workflow confusion

A repository name can change or be reused. A valid repository may attempt to run an unapproved workflow, ref, event or environment.

Policy must match the immutable `repository_id`, exact `workflow_ref` and exact `environment` claims. Human-readable names may appear in diagnostics but must not be an authorisation key. Swarrow must not derive authority by parsing GitHub's default `sub` format.

### Reusable workflow confusion

A reusable workflow token describes the calling workflow through the standard workflow claims and the called workflow through `job_workflow_ref`. Treating those identities as interchangeable could authorise a caller or shared workflow that the operator did not intend.

The initial version must reject tokens containing a `job_workflow_ref` claim. Supporting reusable workflows requires a separate policy model and security review.

### Cross-service or cross-image deployment

A caller may submit another service name, image repository or crafted image reference.

Those values must come from local policy rather than the request. The request should contain a deployment identifier and digest only. The digest parser must reject tags, ambiguous references and unsupported algorithms.

### Unintended service mutation

A full Docker service update can modify far more than the image. Incorrect code could overwrite concurrent operator changes or send caller-controlled fields.

Swarrow must inspect the current service, copy its specification, alter only the image and submit the current version index. Tests must compare every preserved field, and version conflicts must fail rather than overwrite silently.

### Duplicate and concurrent deployment

Retries or concurrent workflows may submit the same or competing digests.

Requests should be idempotent for the same deployment and digest. Updates to one service should be serialised, while a conflicting stale update must produce an explicit result rather than an accidental last-write-wins outcome.

### Malicious image

An authorised repository may deliberately or accidentally deploy malicious code. That code may read the target service's secrets, access attached networks, modify mounted storage or exercise configured Linux capabilities.

This is residual risk inherent in delegating image selection. Operators must configure services using least privilege and should treat repository write and workflow modification rights as production access to that application.

### Denial of service

An attacker may flood authentication, registry or deployment operations, or an authorised workflow may repeatedly trigger rollouts.

The service should use request limits, timeouts, bounded concurrency and per-deployment rate controls. Docker and registry failures must not exhaust unbounded goroutines or memory.

### Controller compromise

Remote-code execution, dependency compromise or unsafe parsing inside Swarrow may expose manager authority.

The implementation should minimise dependencies, avoid shell execution, use the versioned Docker Engine client, run with restricted host permissions beyond its required Docker access and publish reproducible release metadata. Input handling and authorisation code require focused tests and review.

### Information disclosure

Errors, metrics or audit events may reveal tokens, private image names, service configuration or Docker API responses.

Responses should expose only the information required for the caller's configured deployment. Logs must redact credentials and avoid serialising complete service specifications or tokens.

## Explicitly accepted risks

- Swarrow remains a privileged component with access to a Swarm manager.
- A repository authorised to choose an image controls code executed by its target service.
- Existing service privileges determine the blast radius of a malicious image.
- The initial version relies on operator-managed TLS termination and host hardening unless the implementation design later provides them directly.
- Swarrow does not prevent an infrastructure operator from replacing the image through another Docker or stack operation.

## Security review triggers

The threat model must be revisited before adding another identity provider, reusable workflows, optional identity constraints, mutable tags, registry-side automation, topology changes, multi-cluster support, dynamic policy, a browser interface or any generic Docker operation.
