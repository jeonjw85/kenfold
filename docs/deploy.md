# Remote access

By default Kenfold listens on `127.0.0.1` only, which is enough for agents on the same machine. Clients that run elsewhere need a public HTTPS URL:

- ChatGPT and claude.ai (web and mobile) run in their providers' clouds.
- Agents on your other machines.

Kenfold does not terminate TLS itself. Put it behind a tunnel or a reverse proxy, and tell it the public URL:

```sh
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up    # or make up-embed / up-extract
```

Setting `KENFOLD_PUBLIC_URL` adds its host to the Host allowlist and turns on the built-in OAuth 2.1 authorization server, which is how ChatGPT and claude.ai connect. Agents with an API key keep working as before, locally and through the public URL.

## Before you expose Kenfold

The public URL leads to your whole memory store. Everything is authenticated, but check the following first:

1. **Set the owner password.** It approves OAuth clients on the consent page and logs you in to the [dashboard](#the-dashboard-stays-local). Use a unique password of at least 12 characters (a password manager's is best):

   ```sh
   docker compose exec kenfold /usr/local/bin/kenfold password
   ```

   After five wrong attempts the consent page and the dashboard login lock for 15 minutes. Repeated wrong-password or lockout lines in `make logs` mean someone is guessing.
2. **Keep `KENFOLD_AUTH=apikey`** (the default). OAuth refuses to start without it.
3. **Give each client the least access it needs.** The consent page preselects read-only; choose "read and write" only for clients that should store memories. With write access, a client can change what your other agents are told.
4. **Approve only connections you started.** The consent page shows the client's id and the host you are sent back to after approval (for ChatGPT, `chatgpt.com`; for claude.ai, `claude.ai`). Deny if either is unexpected: anyone can name a client "ChatGPT" and send you a link.
5. **Back up the database** (the `pgdata` volume) if losing memories would hurt.

## The dashboard stays local

Setting `KENFOLD_PUBLIC_URL` does not publish the dashboard. `/dashboard/` answers only for `localhost` and loopback addresses, and it refuses requests that carry proxy headers such as `X-Forwarded-For`, so it is not reachable through the public URL or through a tunnel that rewrites the `Host` header. To use it from another machine, forward the port over SSH and open `http://127.0.0.1:7077/dashboard/`:

```sh
ssh -L 7077:127.0.0.1:7077 your-server
```

`KENFOLD_DASHBOARD=remote` also serves the dashboard on the public URL's host. Then only the owner password stands between the internet and your memory, including the power to revoke clients and forget memories. Prefer a URL only you can reach (`tailscale serve`, or an access policy in front of the tunnel). `KENFOLD_DASHBOARD=off` turns the dashboard off.

## Option A: Tailscale Funnel

For a machine with [Tailscale](https://tailscale.com); no domain needed. Funnel publishes a local port at `https://<machine>.<tailnet>.ts.net` with a certificate it manages:

```sh
tailscale funnel --bg 7077            # prints the public URL
KENFOLD_PUBLIC_URL=https://<machine>.<tailnet>.ts.net make up
```

Funnel only proxies to `http://127.0.0.1`, which is where compose publishes Kenfold. Anyone with the URL reaches the consent page. `tailscale funnel reset` stops it.

For your own devices only, `tailscale serve --bg 7077` publishes to your tailnet instead of the internet. ChatGPT and claude.ai cannot reach a tailnet-only URL.

## Option B: Cloudflare Tunnel

For a domain on Cloudflare. Create a named tunnel once:

```sh
cloudflared tunnel login
cloudflared tunnel create kenfold
cloudflared tunnel route dns kenfold kenfold.example.com
```

Put this in `~/.cloudflared/config.yml`:

```yaml
tunnel: <tunnel id printed by create>
credentials-file: /home/you/.cloudflared/<tunnel id>.json
ingress:
  - hostname: kenfold.example.com
    service: http://127.0.0.1:7077
  - service: http_status:404
```

Then start the tunnel and Kenfold:

```sh
cloudflared tunnel run kenfold
KENFOLD_PUBLIC_URL=https://kenfold.example.com make up
```

Avoid quick tunnels (`*.trycloudflare.com`): they get a new URL on every start. OAuth tokens are bound to the public URL, so every client would have to be approved again.

## Option C: Caddy on a server

For a server with a public IP and a DNS name pointing at it. `deploy/compose.caddy.yaml` adds [Caddy](https://caddyserver.com), which obtains and renews the certificate itself (ports 80 and 443 must be reachable):

```sh
export KENFOLD_DOMAIN=kenfold.example.com KENFOLD_PUBLIC_URL=https://kenfold.example.com
docker compose -f compose.yaml -f deploy/compose.caddy.yaml up -d --build --wait
```

Only Caddy's ports are published on the network; Kenfold stays on `127.0.0.1:7077` and the compose network.

Any other reverse proxy works if it:

- terminates TLS;
- passes the `Host` header through (or you add its value to `KENFOLD_ALLOWED_HOSTS`);
- does not buffer `text/event-stream` responses;
- serves Kenfold at the root of the host, not under a path prefix.

## Connect ChatGPT

Developer mode is available on Pro, Plus, Business, Enterprise, and Education accounts on the web:

1. In ChatGPT, turn on developer mode under **Settings → Security and login**.
2. Create an app on the ChatGPT Plugins page (the + button) with the URL `https://kenfold.example.com/mcp` and OAuth authentication.
3. ChatGPT discovers Kenfold's authorization server and opens the Kenfold consent page. Kenfold supports both client registration methods ChatGPT offers:
   - its client metadata document, whose `private_key_jwt` client authentication Kenfold verifies against ChatGPT's published keys;
   - dynamic registration.
4. On the consent page:
   - choose the access level;
   - keep or change the agent name (`chatgpt`), which is recorded on every memory it writes;
   - enter the owner password.

ChatGPT asks you to confirm write actions (`remember`, `handoff`, `resume`, `forget`). Kenfold's read-only tools (`get_context`, `recall`) are marked as such, so they run without confirmation.

## Connect claude.ai

Add a custom connector with the URL `https://kenfold.example.com/mcp`. Claude identifies itself with its client metadata document (a public client that redirects to `claude.ai`), which Kenfold accepts. Then the consent page opens as for ChatGPT.

## Other machines

Agents with API keys use the public URL instead of `http://127.0.0.1:7077/mcp`. That covers Claude Code, Codex, and OpenCode, and the session hook via `KENFOLD_URL`. Keys are managed as before, with `make key` on the server.

## Managing clients

```sh
docker compose exec kenfold /usr/local/bin/kenfold oauth clients         # approved clients, access, last use
docker compose exec kenfold /usr/local/bin/kenfold oauth revoke <grant>  # cut one off (its tokens stop working immediately)
docker compose exec kenfold /usr/local/bin/kenfold password              # change the owner password (ends dashboard sessions)
```

The dashboard's Clients page (`/dashboard/clients`) lists and revokes OAuth clients and API keys as well.

- **Token lifetimes.** Access tokens last an hour and refresh tokens 30 days. A refresh token works once: presenting a used one revokes the client's grant, because it means someone else has a copy.
- **Changing `KENFOLD_PUBLIC_URL`** invalidates all OAuth tokens, since they are bound to it. Clients have to connect again.
- **Dynamic registration** can be turned off with `KENFOLD_OAUTH_DCR=false`. Registrations that never get approved are deleted after a day.

## What was tested

Kenfold was run locally behind the shipped `deploy/Caddyfile`, with Caddy's internal CA, at `https://kenfold.localhost:8443`. The following worked through the proxy:

- the 401 challenge and discovery metadata;
- dynamic registration, the consent page, the PKCE code exchange, and refresh rotation;
- MCP calls with a read-and-write and a read-only grant;
- revocation from the CLI.

The integration tests cover the rest:

- the MCP Go SDK's OAuth client, with a client metadata document and with dynamic registration;
- a `private_key_jwt` client, including wrong keys, audiences, expiry, and replay.

The metadata documents ChatGPT and Claude publish, and ChatGPT's key set, were fetched with Kenfold's production fetcher and accepted.

Tailscale Funnel, Cloudflare Tunnel, and connections from the real ChatGPT and claude.ai were not tested. They need a public URL and accounts.
