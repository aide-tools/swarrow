# Swarrow

Swarrow is a small deployment relay for Docker Swarm. It lets an authorised GitHub Actions workflow update the image of one preconfigured service without giving the workflow SSH access or access to the Docker API.

> [!WARNING]
> Swarrow is being designed and is not ready for use. The security contract is documented before implementation so that its trust boundary can be reviewed explicitly.

## Motivation

A Docker Swarm operator may manage service topology centrally while individual application repositories build and publish their own container images. Most application releases only need to replace the image of an existing service.

The usual automation options grant considerably more authority than that task requires. SSH access to a manager, membership of the `docker` group and direct access to the Docker API can all control the wider host or swarm.

Swarrow is intended to provide one narrow handoff:

```text
GitHub Actions workflow
        │ authenticated deployment request
        ▼
     Swarrow
        │ preconfigured service and image repository
        ▼
Docker Swarm service
```

The workflow supplies an immutable image digest. Swarrow determines whether the workflow may use that deployment target, constructs the approved image reference and updates the existing service.

## Security boundary

An authorised workflow may:

- Deploy an immutable digest from one approved image repository
- Update the image of one preconfigured Swarm service
- Observe the result of that deployment

It may not choose another service or image repository, alter the service's configuration or make arbitrary Docker API requests.

Changing a service's image still allows the application repository to run arbitrary code with that service's existing secrets, networks, storage and privileges. Swarrow limits control-plane authority; it does not sandbox the application workload.

## Design

The initial design is documented in:

- [Design](docs/design.md)
- [Threat model](docs/threat-model.md)
- [Illustrative configuration](docs/configuration.md)

These documents describe the intended contract rather than an implemented or stable API.

## Licence

Swarrow is licensed under the [Apache License 2.0](LICENSE).
