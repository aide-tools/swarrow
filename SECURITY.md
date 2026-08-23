# Security policy

## Supported versions

Before Swarrow reaches 1.0, only the latest release is eligible for security fixes. Reports against `main` are welcome when the issue is still reproducible, but fixes are not routinely backported to older pre-release versions.

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability. Submit a [private vulnerability report](https://github.com/aide-tools/swarrow/security/advisories/new) instead.

Include enough information to assess the report safely:

- The affected version or commit
- The impact and any required preconditions
- Steps or a minimal example that reproduce the issue
- A suggested mitigation or fix, when available

Do not include credentials, tokens or unrelated sensitive data. The maintainer will coordinate a fix and public disclosure with the reporter. No response or remediation service level is currently offered.

## Scope

Relevant reports include authentication or authorisation bypasses, replay protection failures, unintended Docker service changes, access to unconfigured services or image repositories and disclosure of sensitive deployment information.

Swarrow is a privileged component and does not sandbox deployed workloads. An authorised workflow can deploy code with the configured service's existing secrets, networks, storage and privileges. The [threat model](docs/threat-model.md) documents this boundary and other explicitly accepted risks; those properties are not vulnerabilities by themselves.
