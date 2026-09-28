# ADR-0003: Remote access and OAuth

- Status: accepted
- Date: 2026-09-28

## Context

Phase 4 of [ADR-0001](0001-architecture.md) targets clients that run in their providers' clouds, ChatGPT and claude.ai, and agents on other machines. Such clients need a public HTTPS endpoint. They cannot be handed an API key: they authenticate the way the MCP authorization specification (2026-07-28) defines, which means:

- **Protected Resource Metadata** (RFC 9728), advertised in the 401 challenge.
- An **OAuth 2.1 authorization server** with PKCE.
- **Resource indicators** (RFC 8707): tokens are bound to the MCP server they were issued for.
- **Client identification**, preferably by a Client ID Metadata Document: the client_id is an https URL whose JSON describes the client. Dynamic registration (RFC 7591) is kept for older clients.

Exposing Kenfold exposes every memory, so the design is judged by what an attacker who can reach the URL can do.

## Decisions

### A built-in authorization server for one owner

Kenfold ships its own authorization server (`internal/oauth`) rather than depending on an external identity provider:

- A personal deployment has no IdP to delegate to.
- The consent step needs Kenfold-specific choices:
  - the access level;
  - the agent name recorded as `source_agent` on the client's writes.

There are no user accounts. The owner approves a client on a consent page by entering the owner password (`kenfold oauth password`, stored as Argon2id). After five wrong attempts the page locks for 15 minutes; the count is kept in the database, so it survives concurrent requests and restarts.

The page is protected against:

- framing (`frame-ancestors 'none'`);
- scripts (a CSP that allows none);
- cross-site posts (Go's cross-origin protection);
- tampering: the validated request is sealed into the form with an HMAC and a 10-minute expiry, so the browser can only contribute the owner's choices.

An external authorization server could be supported later by pointing the Protected Resource Metadata at it and validating its tokens. It is not needed for one owner.

### Clients: metadata documents first, public or key-authenticated

- **Metadata documents** are fetched with an SSRF guard, because the URL is chosen by whoever starts an authorization:
  - https on the default port only;
  - public unicast addresses only, checked at dial time so DNS rebinding cannot bypass it;
  - no redirects, a 5 s timeout, 64 KiB, and the document's `client_id` must equal its URL.
  - Documents are cached for a day.
- **Dynamic registration** is on by default (`KENFOLD_OAUTH_DCR=false` disables it) and registers public clients only. Registrations never approved are deleted after a day.
- **Client authentication** at the token and revocation endpoints is either:
  - `none`: public clients; PKCE authenticates the code exchange; or
  - `private_key_jwt` (RFC 7523): a signed assertion verified against the key set the client publishes.

  Client secrets are not supported: there is nothing to share and nothing to leak.

Compatibility was checked against the documents ChatGPT and Claude actually publish:

- **ChatGPT** declares `private_key_jwt` with RS256 and a `jwks_uri`. Its document and key set parse, and assertions are verified with standard library cryptography: RS256, PS256, ES256, P-256 and RSA keys of 2048 bits or more. Replay protection is by `jti`, and a JWK's declared `alg` is enforced.
- **Claude** declares a public client, and also lists the JWT bearer grant, which Kenfold does not implement. Grant types beyond the authorization code flow are dropped rather than rejected; the token endpoint refuses them anyway.

A first version accepted only `none` and exactly the grants it implements. It would have rejected both clients.

### Tokens: opaque, hashed, audience-bound, rotating

- Access tokens (`kfa_`, 1 hour) and refresh tokens (`kfr_`, 30 days) are random strings. Only their SHA-256 is stored, like API keys.
- **Opaque, not JWTs.** Revoking a grant (`kenfold oauth revoke`) takes effect on the next request, and one indexed lookup per request is cheap at this scale.
- **Audience binding.** Each token is bound at issuance to the canonical resource, `KENFOLD_PUBLIC_URL` + `/mcp`. Resource indicators for anything else are refused, and tokens recorded for another resource are rejected.
- **Single-use codes.** Authorization codes are single use. Presenting one twice revokes its grant, since it may have leaked.
- **Refresh rotation.** Refresh tokens rotate on every use. Presenting a rotated one revokes the grant and every token under it. Either the client or a thief has a copy, and the owner has to approve the client again.
- Distinct prefixes make leaked tokens recognizable. The secret filter rejects them in memories.

### Access levels are enforced by the tools

Scopes are `memory:read` and `memory:write`, and write implies read. The consent page preselects read-only, and offers write only when the client asked for it.

A read-only grant can list and call every tool. The write tools (`remember`, `handoff`, `forget`, and `resume`, which records who picked up a handoff) return a tool error that explains why. The REST API's write endpoint returns 403.

Enforcing in the tools keeps discovery uniform and gives the model a message it can act on. It also matches ChatGPT's handling, which asks the user to confirm tools without a read-only annotation.

### TLS outside Kenfold

Kenfold does not terminate TLS. A tunnel (Tailscale Funnel, Cloudflare Tunnel) or a reverse proxy does; `deploy/` has a Caddy example.

`KENFOLD_PUBLIC_URL` must be https (loopback http is accepted for local testing). Its host joins the Host allowlist, which then protects `/mcp`, the REST API, and the OAuth endpoints alike.

The SDK's own DNS-rebinding check is disabled. It only covers loopback listeners, and it rejected the public host of a server behind a tunnel on the same machine; Kenfold's allowlist covers both cases.

### Deferred

- **An OpenAI-compatible proxy for local models.** Open WebUI (0.6.31+) and LM Studio (0.3.17+) are MCP hosts themselves. They connect to `/mcp` with an API key, so a proxy that injects memory into chat completions would duplicate `get_context` with less control. Revisit if a target client cannot speak MCP.
- **Object storage.** Kenfold stores nothing large:
  - memories are capped at 8000 characters and session summaries at 4000;
  - raw transcripts stay on the machine that captured them.

  `memory.evidence_uri` remains the place to reference external evidence once something needs it.

## Consequences

- Anyone who can reach the public URL can reach the consent page. The owner password and lockout bound guessing to about 480 attempts a day. A unique, long password is required reading in `docs/deploy.md`.
- **Phishing.** An attacker can register a client named like a trusted one and send the owner a consent link. The page shows the client id and the host the owner is sent back to. Owners should approve only connections they started themselves.
- **Registration spam.** Dynamic registration and metadata fetches let unauthenticated callers create client rows, deleted after a day if unapproved. There is no rate limiting yet. `KENFOLD_OAUTH_DCR=false` removes registration; metadata fetches remain.
- **Changing `KENFOLD_PUBLIC_URL`** invalidates all OAuth tokens (they are bound to it), and every client must be approved again.
- **Restarts.** The consent form's HMAC key and the assertion replay cache live in memory. A restart invalidates open consent pages. A replayed assertion is still limited by its `exp` (at most an hour) and by the single-use code and rotating refresh token it would be presented with.
- **Real clients untested.** Connections from the real ChatGPT and claude.ai were not tested; that needs a public deployment and accounts. What was verified:
  - their published metadata documents and ChatGPT's key set, with the production fetcher;
  - both registration paths with the MCP Go SDK's OAuth client;
  - `private_key_jwt` end to end with a test client;
  - the complete flow over HTTPS behind Caddy.
