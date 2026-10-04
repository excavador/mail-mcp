# mail-mcp

[Model Context Protocol](https://modelcontextprotocol.io) server for
mailboxes reached over IMAP: Gmail directly (app password, implicit TLS on 993),
Proton through [Proton Mail Bridge](https://proton.me/bridge) (STARTTLS on 143,
with self-signed certificate pinning by SHA-256).

```
ghcr.io/excavador/mail-mcp              image, multi-arch amd64 + arm64
oci://ghcr.io/excavador/charts/mail-mcp chart
```

## Architecture

mail-mcp serves two MCP endpoints from one process, because [sluis](https://github.com/truvity/sluis) grants sessions longer than 24 hours only to read-only resources (sluis ADR 0033). Reading mailboxes should not require daily sign-in; organising happens rarely and can.

| endpoint | tools | session | audience |
|---|---|---|---|
| `/mcp` | read only | 7 days | read-only resource |
| `/mcp-admin` | read + write | 24 hours | write resource |

Both endpoints are served over HTTP and validate bearer tokens themselves: minted
by the issuer, checked against its JWKS with the endpoint's own resource URL
as the required audience (RFC 8707). `/healthz` is unauthenticated liveness,
deliberately touching no mailbox — a probe that fails when Gmail blips takes
the pod out of service for something a restart cannot fix. stdio transport
serves the admin tool set for local use.

## Configuration

An accounts YAML file, one entry per mailbox. Passwords live in separate files,
never in the config — so the accounts file can be shown, logged and diffed
without leaking credentials.

```yaml
accounts:
  - name: gmail-work
    provider: gmail
    host: imap.gmail.com
    port: 993
    tls: implicit
    username: user@gmail.com
    passwordFile: /secrets/gmail-work-password
  - name: proton-main
    provider: proton
    host: 127.0.0.1
    port: 143
    tls: starttls
    username: user@protonmail.com
    passwordFile: /secrets/proton-password
    pinnedCertSHA256: ab12cd34...
```

To compute the Bridge certificate pin:

```bash
openssl s_client -starttls imap -connect 127.0.0.1:143 </dev/null | \
  openssl x509 -outform DER | sha256sum
```

### Flags and environment variables

| flag | env | required | mode |
|---|---|---|---|
| `--accounts` | `ACCOUNTS_FILE` | yes | all |
| `--transport` | `TRANSPORT` | no; default `stdio` | all |
| `--addr` | `ADDR` | no; default `0.0.0.0:8080` | http |
| `--issuer-url` | `ISSUER_URL` | yes | http |
| `--read-resource-url` | `READ_RESOURCE_URL` | yes | http |
| `--admin-resource-url` | `ADMIN_RESOURCE_URL` | yes | http |
| `--scope` | `SCOPE` | no; default `openid` | http |

The read and admin resource URLs must differ — that difference is the whole
point, so that sluis can mint 7-day tokens for the read endpoint and shorter
tokens for the write endpoint.

## Tools

### Current (v1 in progress)

| tool | |
|---|---|
| `list_accounts` | List the mailboxes this server reaches, with the provider (Gmail or Proton) and how each organises messages |
| `list_folders` | List every folder (Gmail: label) in one account, with how many messages each holds |

### Coming

v1 will add organising tools and history, guarded by intent approval:

- **`search`** — full-text search over the immutable, content-addressed cache
- **`fetch_message`** — retrieve one message for reading or forwarding
- **`sender_stats`** — when was the last message from a person, how many have you sent to them
- **`create_folder`**, **`apply`**, **`undo`**, **`reapply`** — move and label messages with a history of intents, so a mistake can be rolled back

## What it will not do

**There is no delete tool, and there will not be one.** Moves are reversible; deletes are not. The only way to throw a message away is to move it, so you can undo it later.

## Two things to know before deploying it

**The HTTP transport validates every request itself.** A bearer token minted
by [sluis](https://github.com/truvity/sluis), checked against its JWKS with
this server's own resource URL as the required audience (RFC 8707). Both
`ISSUER_URL` and the resource URLs are required — there is no way to start
the http transport or render the chart without them, and no gateway fallback.

**Proton Mail Bridge presents a self-signed certificate and mail-mcp enforces
a pin on it — there is no "skip verification" setting at all.** The honest
answer to a self-signed certificate is to pin it, not to switch verification
off and hope nothing else answers on the port. Gmail's certificate is verified
against the system roots.

## Licence

MIT, © 2026 Oleg Tsarev.
