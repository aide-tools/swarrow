# Repository instructions

## Start with context

- Confirm that the working directory is the `aide-tools/swarrow` repository.
- Read `README.md` and the documents relevant to the change before editing. `docs/design.md` owns the product model and security invariants, `docs/threat-model.md` owns threats and accepted risks, `docs/configuration.md` owns the file schema, `docs/http-api.md` owns the wire contract and `docs/deployment.md` owns operator guidance.
- Inspect the working tree, active branch, recent commits and live GitHub state for any referenced pull request or issue.
- Treat an existing plan or requested direction as a hypothesis. Confirm that the problem still exists and prefer the smallest change that solves it.
- Do not begin a later implementation slice while the user is still reviewing the current pull request.

## Preserve Swarrow's boundary

- Swarrow is a narrow deployment relay, not a general deployment system or Docker API proxy.
- The caller may select a configured deployment and supply an immutable image digest. It must not select a Docker service, image repository or any other service setting.
- Infrastructure configuration owns topology, networks, storage, secrets, placement, resources, replica counts and rollout behaviour. Swarrow changes only the image of an existing service.
- Swarrow is privileged because it accesses a Swarm manager. Container restrictions reduce unrelated privilege but do not make Docker access safe or sandbox the deployed workload.
- Denied requests must not reach Docker.
- Authentication, authorisation, replay handling, concurrency, image validation and Docker update semantics are security-sensitive. Resolve their design and threat-model implications before implementation.
- Do not add mutable image tags, another identity provider, optional identity constraints, dynamic policy or generic Docker operations without an explicit design review.

## Keep configuration explicit

- Decode configuration strictly, reject unknown fields and fail closed when required policy is missing.
- Keep GitHub.com's issuer fixed unless a reviewed design deliberately changes that boundary.
- Do not introduce Viper, configuration overlays or reflection-based validation without a compelling reviewed reason. The current explicit decoding and validation are intentional.
- Human-readable repository names are diagnostic only. Authorisation uses the immutable repository ID and exact workflow, reusable-workflow and environment policy described in `docs/design.md`.

## Implementation guidance

- Use the Go version declared in `go.mod` and `mise.toml`, normally through `mise exec --`.
- Prefer the standard library and existing dependencies. New dependencies require a concrete benefit, especially in authentication, parsing and Docker-facing code.
- Keep Cobra command construction local rather than using package-global command state.
- Prefer semantic names and straightforward control flow over compact or clever abstractions.
- Keep request bodies, replay storage, queues and timeouts bounded. Fail closed when a safety limit is reached.
- Preserve the inspected Docker service specification and version index. An image update must not silently overwrite another service change.
- Never log complete identity tokens, credentials or complete service specifications.

## Tests and validation

- Put tests in the same commit as the behaviour they verify.
- Keep every commit buildable and green.
- Run focused tests while working, then run the applicable final checks:

```sh
mise exec -- go mod verify
mise exec -- gofmt -l .
mise exec -- go vet ./...
mise exec -- go test ./...
```

- The formatting check must produce no output.
- Run `mise exec -- go test -race ./...` for concurrency, replay, deployment coordination, server or Swarm observer changes.
- Run `mise exec -- go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` after changing GitHub Actions workflows.
- Build both supported Linux architectures and the multi-platform image after changing packaging or release behaviour.
- Do not claim live Swarm validation when only fake clients or unit tests were used.

## Pull requests and commits

- Prefer small pull requests with one clear outcome.
- Keep one idea per commit. Behaviour and its tests belong together; tooling, documentation and mechanical refactors normally belong in separate commits.
- When implementation changes require documentation, place the documentation commit at the tip of the pull request.
- Fold refinements into the idea they complete unless preserving them separately materially improves review.
- Rewrite published history only with explicit approval and use `--force-with-lease`.
- Do not push, merge, post comments or otherwise mutate GitHub state unless explicitly authorised.
- Write pull request descriptions in plain language. Explain unfamiliar GitHub Actions, OIDC, Docker and Swarm concepts instead of assuming specialist knowledge.
- Put related GitHub issues, pull requests and discussions in a `Related` section. Put repository, release and external links in a `References` section.

## Documentation style

- Use British English, direct prose and restrained formatting.
- Do not hard-wrap Markdown prose.
- Begin sentence-like list items with capitals.
- Explain what a mechanism does before explaining why its security properties matter.
- Keep design rationale in `docs/design.md`, threats in `docs/threat-model.md`, schema details in `docs/configuration.md`, HTTP behaviour in `docs/http-api.md` and operational instructions in `docs/deployment.md`.
- Treat Atelier, Mailbridge and other installations as validation evidence, not normative configuration. Do not copy host-specific identities, domains, group IDs or rate limits into general guidance.
