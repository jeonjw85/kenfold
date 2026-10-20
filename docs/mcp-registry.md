# Official MCP Registry readiness and publishing

Checked on 2026-10-20 against the official Registry documentation and registry
source commit `11b7b0e274d518b6f449c5ee3518a6151d9848b9`.
This change prepares metadata; it does not publish a listing.

## Current decision

Kenfold can be submitted as a **metadata-only, source-install listing** under
`io.github.jeonjw85/kenfold`, after the namespace owner authenticates. The current
2025-12-11 schema does not require `packages` or `remotes`, and the official
validator explicitly accepts their absence. The prepared root `server.json`
passed the live official `POST /v0.1/validate` endpoint on 2026-10-20 with
`{"valid":true,"issues":[]}`. Validation is not publication, namespace ownership
verification, protocol certification, or a promise of downstream catalog display.
Consumers that require an installable package or remote endpoint may omit it.

`websiteUrl` points to the source/Compose quickstart. No public package or hosted
service is claimed. The version is the latest existing release, `0.1.2`, rather
than a newly invented release. Before submission, recheck the latest release and
whether this name/version already exists. Published version metadata is immutable;
use a new unique version for later metadata changes.

## Implementation versus registration requirements

| Area | Kenfold evidence | Registry consequence |
| --- | --- | --- |
| MCP implementation | Official Go SDK v1.8.0 in `go.mod`; typed tool registration in `internal/mcpserver/server.go` | An actual MCP server, not just a REST API or a skill document |
| Local transport | `kenfold mcp`, `mcp.StdioTransport` in `cmd/kenfold/serve.go` | Can support a future stdio package; must pass the `mcp` argument, because the container defaults to `serve` |
| HTTP transport | Stateless `mcp.NewStreamableHTTPHandler` at `/mcp` in `internal/httpserver/httpserver.go` | Correct `streamable-http` transport; do not describe it as legacy SSE |
| HTTP authentication | Default per-agent Bearer API keys; optional OAuth authorization code + PKCE S256, protected-resource/authorization-server discovery, consent and read/write scopes in `internal/oauth` | Authentication is supported; a listing never contains actual keys or owner credentials |
| stdio authentication | Direct database access, configured agent identity; no HTTP API key verification | Mark a future database URL as secret; local users must be trusted to access that database |
| Deployment | Source build and local Docker Compose; PostgreSQL 18 + pgvector, migrated schema required; models optional | Registry stores metadata, not source builds or database provisioning |
| Released artifacts | v0.1.2 GitHub release has no attached assets; current Compose builds locally; no public image is referenced by this repository | No installable `packages` entry is asserted; a Git source archive is not an MCPB package |
| Remote endpoint | README uses loopback or operator-supplied domains; no project-operated public endpoint is documented | Omit `remotes`; do not list localhost, example domains, or a private memory instance |
| Identity/metadata | Root `server.json`: schema, namespace/name, title, description (under 100 characters), version, repository, website | Owner must authenticate to publish; no external listing agency is needed |
| Other catalog files | No root `SKILL.md` or `llms.txt` | Neither file is required by the official MCP Registry |

The existing protocol/authentication tests and documentation provide implementation
evidence, not proof that every hosted client works. The v0.1.2 release explicitly
notes that real ChatGPT and claude.ai connections have not been tested. This PR
changes no Go code, database schema, authentication policy, or network exposure.

## Optional: make the listing automatically installable

A public OCI image is a natural next step for this Go project, but is **not a
prerequisite for the metadata-only entry**. The Dockerfile now includes the
ownership label required by the Registry:

```dockerfile
LABEL io.modelcontextprotocol.server.name="io.github.jeonjw85/kenfold"
```

Before adding an OCI `packages` entry:

1. Build and test a release image, including `docker run --rm -i IMAGE mcp`.
   Keep stdin open and do not allocate a TTY. The image's default `serve` command
   is HTTP, not stdio. Verify initialize, tools/list and a real read/write operation
   against a dedicated test database.
2. Publish the image to a supported public registry controlled by the maintainer
   (for example GHCR). Confirm anonymous pulls, the ownership label on the
   published image, architectures, and an immutable release tag or digest.
3. Provision PostgreSQL + pgvector separately and run migrations before starting
   stdio clients. A container's localhost is not the host's Compose database.
   Document a reachable database hostname/network for each supported platform.
4. Add `registryType: oci`, the actual published image `identifier`,
   `runtimeHint: docker`, `transport: {"type":"stdio"}`, a positional
   `packageArguments` value of `mcp`, and runtime arguments needed for stdin,
   environment forwarding and the documented network. Describe
   `KENFOLD_DATABASE_URL` as required/secret and `KENFOLD_AGENT` as configurable.
   Optional model endpoints must also be reachable from the container.
5. Validate the final metadata and test the exact generated launch command.
   Adding a label to source alone does not verify an unpublished image.

Alternatively, a real MCPB bundle attached to a GitHub release can be listed with
its SHA-256. Plain Go binaries/source archives are not automatically MCPB bundles;
Go modules are not a supported package registry type.

A remote entry is appropriate only if a maintainer intentionally operates a
publicly reachable service. Configure TLS, `KENFOLD_PUBLIC_URL`, Host protection,
OAuth/keys and owner onboarding first; see [deployment](deploy.md). Model an API
key as a secret input (including the `Bearer` prefix), or use client OAuth
discovery. Do not embed tokens, disable authentication for catalog crawlers, or
publish a personal memory server merely for discovery. URL templates do not
replace the requirement for a real accessible service.

## Maintainer submission procedure

Use the official [quickstart](https://modelcontextprotocol.io/registry/quickstart)
to install a current `mcp-publisher` release from
[modelcontextprotocol/registry](https://github.com/modelcontextprotocol/registry/releases).
From the repository root:

```sh
# Validate without logging in or publishing.
mcp-publisher validate

# Check existing listings before choosing a version.
curl --fail-with-body 'https://registry.modelcontextprotocol.io/v0.1/servers?search=io.github.jeonjw85%2Fkenfold'

# Performed by the namespace owner, using the official GitHub device flow.
mcp-publisher login github

# Explicit publication step, after reviewing server.json.
mcp-publisher publish

# Verify the published record.
curl --fail-with-body 'https://registry.modelcontextprotocol.io/v0.1/servers/io.github.jeonjw85%2Fkenfold/versions/0.1.2'
```

The owner must authenticate as `jeonjw85` for this personal GitHub namespace.
Kenfold's own OAuth login is unrelated to Registry publisher authentication.
Never commit publisher tokens or forward device codes, GitHub tokens, database
URLs or owner passwords to a catalog operator. Automated publishing can later
use the official GitHub Actions OIDC flow; this change intentionally adds no
publication workflow or new CI write permissions. Merging this PR does not
publish anything, and a PR to the Registry repository does not create a listing.

## Verification and limits

- Live official validation of the exact root JSON: `valid: true`, no issues.
- Official source also explicitly tests nil/empty packages and remotes:
  [validator tests at the inspected revision](https://github.com/modelcontextprotocol/registry/blob/11b7b0e274d518b6f449c5ee3518a6151d9848b9/internal/validators/validators_test.go).
- The validation endpoint checks schema and semantic rules; it does not run MCP
  tools or authenticate namespace ownership. Package ownership is checked during
  publication if a package is supplied.
- Go and Docker were unavailable in the preparation environment. No runtime,
  integration, production image build or OCI-label inspection was run locally.
  Existing release-check CI builds the production container on PRs.

## Issue #5 and third-party listing offers

The offer can at most save manual work across independently verified catalogs;
it cannot bypass namespace ownership, package ownership, release engineering or
client compatibility checks. No evidence of its reliability or successful
submissions was established here. For the official Registry alone, these files
and the owner's official login are sufficient preparation; granting an agency
repository access or sending private data has no demonstrated benefit. A public
fork-based PR needs no repository write access. Evaluate any additional catalogs
individually and ask for exact destinations and verifiable resulting links before
authorizing submissions. No offer was accepted and no catalog submission was
performed by this change.

## Official references

- [Current schema](https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json)
- [Official Registry requirements](https://github.com/modelcontextprotocol/registry/blob/main/docs/reference/server-json/official-registry-requirements.md)
- [Package types and ownership verification](https://modelcontextprotocol.io/registry/package-types)
- [Remote servers](https://modelcontextprotocol.io/registry/remote-servers)
- [Publisher authentication](https://modelcontextprotocol.io/registry/authentication)
- [Versioning](https://modelcontextprotocol.io/registry/versioning)
