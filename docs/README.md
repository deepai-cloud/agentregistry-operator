# Documentation

| Goal | Guide |
| --- | --- |
| Run a registry locally | [Quickstart](quickstart.md) |
| Install into an existing cluster | [Installation](installation.md) |
| Create a registry with managed or external PostgreSQL | [Creating a registry](creating-a-registry.md) |
| Configure every supported API field | [API reference](api-reference.md) |
| Upgrade, rotate credentials, delete or troubleshoot | [Operations](operations.md) |
| Understand authentication and tenant isolation | [Security and tenancy](security.md) |
| Copy runnable manifests | [Examples](../examples/README.md) |
| Configure the Helm chart | [Chart reference](../charts/agentregistry-operator/README.md) |
| Build and publish installable artifacts | [Packaging and releases](releases.md) |

The Helm chart installs the control plane. Each `AgentRegistry` installs one
upstream catalog with an authenticated UI, API and MCP endpoint, and provisions
persistent PostgreSQL by default. An existing external database remains an option.
Storage infrastructure, backups, authentication Secrets and public TLS/DNS remain
platform responsibilities.
