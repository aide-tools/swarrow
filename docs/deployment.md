# Deployment

Swarrow is published as a multi-platform Linux image at `ghcr.io/aide-tools/swarrow`. Release images support `linux/amd64` and `linux/arm64` and include no shell or package manager.

Every `vMAJOR.MINOR.PATCH` tag on `main` publishes an image tagged `MAJOR.MINOR.PATCH`, without the leading `v`. The GitHub release contains a text file with the immutable image reference. Production infrastructure should use that digest rather than the mutable version tag.

Release images include OCI SBOM and provenance metadata and a GitHub artifact attestation. Verify an image with:

```sh
gh attestation verify oci://ghcr.io/aide-tools/swarrow@sha256:<digest> -R aide-tools/swarrow
```

## Swarm service

Swarrow must run on a manager and access its local Docker socket. The image runs as UID and GID `65532` by default. Because `docker stack deploy` does not support supplementary groups, set its primary GID to the numeric group that owns the manager's Docker socket.

This is a minimal service shape, not a complete infrastructure policy:

```yaml
version: "3.8"

services:
  swarrow:
    image: ghcr.io/aide-tools/swarrow@sha256:<release-digest>
    command:
      - serve
      - --config
      - /swarrow.yaml
    user: "65532:${SWARROW_DOCKER_SOCKET_GID:?set SWARROW_DOCKER_SOCKET_GID}"
    read_only: true
    cap_drop:
      - ALL
    volumes:
      - type: bind
        source: /var/run/docker.sock
        target: /var/run/docker.sock
    configs:
      - source: swarrow
        target: /swarrow.yaml
    networks:
      - proxy
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.role == manager

configs:
  swarrow:
    file: ./swarrow.yaml

networks:
  proxy:
    external: true
```

On Linux, obtain the socket GID and deploy the stack with:

```sh
SWARROW_DOCKER_SOCKET_GID="$(stat -c '%g' /var/run/docker.sock)" docker stack deploy --compose-file swarrow-stack.yaml swarrow
```

Swarm configs are immutable. When `swarrow.yaml` changes, rotate the config name in both the service mount and the top-level `configs` declaration before deploying the stack again. Remove the old config only after no service references it.

Use `0.0.0.0:<port>` for `server.listen` inside the container so the reverse proxy can reach it over the private overlay network. Do not publish the plain HTTP port directly. Configure HTTPS, connection and request-rate limits on the Swarrow route, preserve the `Authorization` header and confirm that the response timeout is either disabled or greater than `server.request_timeout` without changing unrelated routes.

GitHub OIDC authenticates deployment requests. Do not place an interactive sign-in flow in front of this machine-facing route because a GitHub Actions job cannot complete a browser challenge.

The socket grants manager-level Docker authority regardless of the process UID. The non-root UID, read-only filesystem and dropped capabilities reduce unrelated container privileges, but they do not make Docker access safe if Swarrow itself is compromised. Run exactly one replica because replay state and service queues are process-local in the initial version.

`docker stack deploy` ignores the Compose `security_opt` field, so this example does not claim to apply `no-new-privileges`. Apply additional host or runtime hardening through controls the deployment platform supports, and verify the resulting live service specification.

## Application workflow

The deployment job must run on the GitHub Actions environment named by Swarrow policy and have `id-token: write` permission. A preceding job should publish the application image and expose its immutable registry digest.

Use the maintained [Swarrow Deploy action](https://github.com/aide-tools/swarrow-deploy) to obtain the GitHub OIDC token, submit the deployment and handle Swarrow's restart warm-up response. Pin the action to a full commit SHA so the workflow executes an immutable version:

```yaml
concurrency:
  group: production-deploy
  cancel-in-progress: false
  queue: max

jobs:
  deploy:
    needs: publish
    runs-on: ubuntu-latest
    timeout-minutes: 10
    environment: production
    permissions:
      id-token: write
    steps:
      - name: Deploy immutable image
        uses: aide-tools/swarrow-deploy@886b3ab96017a9d6422459e7e6b2ddce82adc8d8 # v1.0.0
        with:
          url: https://deploy.example.net
          audience: https://deploy.example.net
          deployment: example-web
          digest: ${{ needs.publish.outputs.digest }}
```

The action treats only `200 OK` as a successful release. It automatically handles one `authentication_warming_up` response by respecting `Retry-After` and requesting a new OIDC token before retrying. A `202 Accepted` response fails the step because Swarrow stopped observing while the rollout remained in progress, leaving an operator or deliberate retry policy to decide what happens next. The [action documentation](https://github.com/aide-tools/swarrow-deploy) describes its inputs and diagnostic mode, while the [HTTP API](http-api.md) defines every server result.

This example authorises the workflow containing the `deploy` job directly. If the application workflow calls a reusable workflow that defines the job, configure its complete `job_workflow_ref` as described in the [configuration reference](configuration.md). Swarrow still checks the configured `repository_id`, `workflow_ref` and `environment` independently; trusting a reusable workflow does not authorise every caller of that workflow.

For a private application image, the existing target service must already retain valid registry credentials, normally established by its operator with `docker stack deploy --with-registry-auth`. Swarrow tells Docker to reuse credentials from that service specification; it does not accept, obtain or refresh registry credentials itself.

### Queuing releases

The `concurrency` block above lets one release run at a time, from image publication through deployment. Use the same group for workflows in the repository that deploy to the same target. The group applies only within that repository, so releases from other repositories and manual deployments need separate coordination.

Set both `cancel-in-progress: false` and `queue: max`. The first lets the active release finish. The second keeps up to 100 runs waiting instead of replacing the pending run whenever another arrives. Once the queue is full, GitHub cancels additional arrivals. Check cancelled releases and rerun the appropriate release when space is available. GitHub does not allow `queue: max` with `cancel-in-progress: true`.

The default queue can lose the latest release when a deployment workflow starts after CI finishes, using a `workflow_run` trigger. For example, commits land in order A, B and C:

1. B passes CI and starts publishing while it is still the latest commit.
2. C lands, passes CI and waits for B to finish.
3. A's slower CI finishes. Its deployment run replaces C in the single pending slot.
4. B finishes. A starts, checks `main` and skips itself because it is out of date. C never deploys.

Keeping more pending runs prevents this replacement while the queue has space. Runs are processed in the order they start waiting, which may differ from commit order or the order workflows were triggered. See [GitHub's concurrency documentation](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency) for the queue settings and limits.

### Checking the revision before deployment

For a `workflow_run` deployment that should release only the current `main` revision, accept only successful CI runs for pushes to `main` in the same repository. Once the deployment workflow gets its turn, compare `github.event.workflow_run.head_sha` with the current `main` SHA. If they differ, skip the release. If the lookup fails, stop the workflow.

Check out that exact tested revision and publish its image, keeping the digest returned by the build. Immediately before calling Swarrow, check the revision against `main` again. This check belongs after any wait for environment approval. Deploy the digest only if the revision still matches.

The first check avoids building an image that is already out of date. The second catches changes to `main` during the build or approval wait. `main` can still change between the final check and deployment.

Swarrow processes requests for each service one at a time, updating the image and observing the rollout. The calling workflow decides which revision to release; Swarrow cannot recover a cancelled GitHub run or determine whether a digest represents the latest commit. If Swarrow stops observing before a rollout finishes, check its outcome before starting another release.

## Validate an installation

`GET /healthz` should return `200 OK` through the public TLS route. This proves that the HTTP process is reachable, but it does not contact GitHub or Docker.

The safest first authenticated request supplies the digest already configured on the target service. A successful response reports `action: no_change`, exercising OIDC verification, local policy, Docker access and service selection without starting a rollout.

Next, deploy a new digest and confirm that the action reports a completed rollout with the expected running task count. For stronger assurance that the narrow update contract holds, capture the live service specification before and after this request, remove the image and Docker-managed version fields and compare the remainder. Networks, secrets, mounts, placement, resources and rollout settings should be unchanged.

Restart Swarrow and submit another request during its authentication warm-up window. The action should wait for the server's `Retry-After` interval, obtain a fresh token and retry successfully. This confirms the expected user experience for the conservative replay cutoff described by the [HTTP API](http-api.md).

Swarrow changes an image and follows its rollout; it does not run database migrations. An application that requires migrations must run them separately and keep changes compatible with the old and new application versions during the rollout.

## Updating application stacks

Running `docker stack deploy` for an application after Swarrow changes its live image may replace that digest with the image declared in the stack file. Keep image selection explicit when applying topology changes. One approach is to read the current service image into a required stack variable before deploying:

```sh
EXAMPLE_WEB_IMAGE="$(docker service inspect --format '{{.Spec.TaskTemplate.ContainerSpec.Image}}' example_web)" docker stack deploy --compose-file example-stack.yaml example
```

The corresponding stack file uses `image: ${EXAMPLE_WEB_IMAGE:?set EXAMPLE_WEB_IMAGE}`. Initial service creation and intentional rollback must instead supply an explicit approved digest. Swarrow does not reconcile stack files or retain release history.
