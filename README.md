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
| `--pdf-extractor-socket` | `PDF_EXTRACTOR_SOCKET` | no; empty (default) turns PDF text off | all |
| `--pdf-stage-dir` | `PDF_STAGE_DIR` | no; default `--cache-dir` | all |

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

### Approval mode

`--approval-mode` (`APPROVAL_MODE`, chart `approval.mode`) sets how `apply_intent` is approved. `client` (default) never elicits: approval is the client's own tool-approval prompt, which shows the account, action, source, target and count, and an apply of more than `--max-unelicited-apply` (50) messages is refused, except `label` (and the undo of a Gmail label), whose cap is `--max-unelicited-label` (`MAX_UNELICITED_LABEL`, default 1000): a label only adds, and its undo removes only what it added. The `expect_*` echo fields stay mandatory at either cap. `elicitation` asks the owner through MCP elicitation forms; use it once the Claude Code VS Code extension renders them ([anthropics/claude-code#98978](https://github.com/anthropics/claude-code/issues/98978)).

### Labelling by tag, and undoing a label

`preview_intent` takes `criterion.tag`: the cached messages of `criterion.folder` that carry that local tag (set with `tag_messages`; same name syntax as tags). It is ANDed with the other criteria, may be the only one besides `folder`, and does not change a mailbox. With `action: label` it adds the target label (Gmail label, Proton `Labels/...`) on top of the current folder and moves nothing, so tagged mail can be labelled `Purchases/Imported`, `Purchases/Shipping` and so on in the mailbox itself.

`undo` of a label removes the label from exactly the messages that intent labelled. Messages that already carried the label when the intent ran (checked against the server, not only the cache) are recorded as `already_in_target` and keep it. The undo preview has `action: unlabel`; the history keeps the stable ids (Gmail: `X-GM-MSGID`), so it works after UIDs change. Gmail: `STORE -X-GM-LABELS` on the label folder, and only that: it requires the X-GM-EXT-1 extension and refuses without it, and mail-mcp never expunges on Gmail, so nothing can reach the Trash. Proton, where a label is a `Labels/...` folder: `UID STORE +FLAGS \Deleted` and `UID EXPUNGE` of exactly those UIDs, only in a `Labels/...` folder and only with UIDPLUS (never a plain `EXPUNGE`); if the expunge fails the flag is cleared again. The Proton path has not yet been verified by hand against Bridge, so on client approval its undo is capped at `--max-unelicited-apply` (50), not the label cap.

## PDF attachment text

With a PDF extractor configured, search finds words inside PDF attachments
(an invoice by its number, a contract by a clause), and `fetch_message` and
`get_thread` (`format=full`) return the extracted text. Without one, nothing
about PDFs changes: attachment names, types and sizes only.

**mail-mcp contains no PDF parser, and will not.** Parsing untrusted PDFs in
the server process was removed after a security review (decompression bombs,
page-tree loops, text amplification). The parsing happens in a sidecar
container (`ghcr.io/excavador/mail-mcp-pdftext`: poppler's `pdftotext` behind
a small static Go helper, `cmd/pdftext`) that can be killed at any time.

### Threat model

A PDF is attacker-controlled input, and so is the text that comes out of it.
What this design assumes and does about it:

- **The parser is compromised or runs away.** It runs in its own container
  with a read-only root file system, no capabilities, the default seccomp
  profile, a 512 Mi memory limit (at least the 400 MB address-space cap plus 64 Mi; the chart refuses less, because below it the kernel OOM-kills `pdftotext` on large files and that looks like a bad file) and one CPU. It does not mount the mail
  store at all: it sees one small staging `emptyDir` (64 Mi), read-only, into
  which mail-mcp writes one decoded PDF at a time, and nothing writable but
  the socket directory. It has no credentials: the accounts secret is mounted
  in the main container only, and the pod mounts no service-account token
  (`automountServiceAccountToken: false`; mail-mcp does not use the
  Kubernetes API).
- **One file must not hurt the next.** The helper starts one `pdftotext` per
  request and kills its whole process group after 20 s of wall time. Before
  it execs, the child applies `RLIMIT_CPU` 15 s, `RLIMIT_AS` 400 MB,
  `RLIMIT_NPROC` 1, `RLIMIT_FSIZE` 0 (it cannot write a file), `RLIMIT_NOFILE`
  64 and `RLIMIT_CORE` 0. They are applied by a limit-and-exec re-exec of the
  helper rather than by `prlimit` on a started child, because that would leave
  a window in which the parser runs unlimited.
- **The client cannot steer the helper.** A request is `{"hash": ...}`. The
  hash must be 64 lowercase hex characters; the helper builds the path itself
  under its read-only mount (`pdf/<hh>/<hash>`), does not follow symlinks, and
  accepts only regular files that start with `%PDF-`. Input over 25 MB is
  refused before anything is spawned. At most 2 jobs run at once.
- **Output amplification.** At most 1 MiB is read from `pdftotext` (then it is
  killed), cut on a UTF-8 boundary and flagged `truncated`; only the first 30
  pages are read (`pages_capped`: the file has at least that many). mail-mcp
  keeps at most 256 KiB per file.
- **The text is hostile.** It reaches the model like a body does: control
  characters are stripped, and `fetch_message` / `get_thread` return it under
  `attachment_text`, each fenced in `<untrusted-email-content nonce=...>` with
  the same per-call nonce as the body (a closing tag inside it is defanged),
  capped at 32 KiB per attachment and 64 KiB per message (16 KiB per message in
  `get_thread`, where it counts toward `max_chars`). Search snippets that come
  from a PDF are treated like body snippets.
- **A sick sidecar.** The client has a pool of 2, a 25 s deadline per call
  and a circuit breaker: after 5 consecutive transport failures (the sidecar
  not answering, or answering garbage) it stops calling for a minute and then
  probes. A `failed` or `timeout` answer describes one file and does not count.
  A file that fails at the transport three times, across passes, is recorded as
  `failed`. A file that was being processed when the process stopped is found
  again as one strike, and is recorded as `failed` only at the third, so no file
  can loop and a restart does not condemn a good file. A broken sidecar is told
  apart from a bad file: the helper runs a self-test on a tiny embedded PDF at
  start and answers `unavailable` to every request while it fails, and also for
  an exec or limit failure or a kill it did not send (the OOM killer), but only if it then also fails its self-test (otherwise a healthy helper means the file caused it, and the answer is `failed`); the client
  counts `unavailable` toward the breaker and never records it as a file's
  outcome, except that a file that gets it three times is recorded as `failed`. A full or broken staging directory is likewise a retryable error, not
  a verdict. Errors have
  fixed texts and never echo input. mail-mcp streams the message blob and the
  attachment (never holding either whole), so a large PDF does not threaten
  the main container's memory limit; the attachment copy is bounded at 25 MB and
  checked against its recorded hash.

What it does not protect against: a bug in poppler that gives code execution
inside the sidecar. The container limits are there for that case; the sidecar
can still read the mail blobs it is mounted with. It is not OCR: a scanned PDF
without a text layer yields no text.

### How it works

The `pdf_text` job runs last in the background scheduler, never touches IMAP,
and writes in short transactions like the other jobs. It walks the PDF
attachments (content type `application/pdf` or a `.pdf` name), takes each
decoded attachment out of its message blob (MIME decoding, no PDF code), stages
it at `<stage>/pdf/<hh>/<sha256>` for the sidecar (`--pdf-stage-dir`, default
the cache directory), and removes it again; the stage is emptied at start. The
outcome of every file (`ok`, `timeout`, `too_large`, `failed`, `not_pdf`) is
cached in the `pdf_text` table by the file's SHA-256, so a hostile file is never
retried and a PDF forwarded a hundred times is extracted once. The text is
written into `attachment_fts`, which search already queries, so no query syntax
changes. `cache_status` reports it as `pdf_text_job`. New mail is picked up by
a rescan every 10 minutes.

### Enabling it

Helm chart:

```yaml
pdfExtractor:
  enabled: true
```

This adds the `pdftext` sidecar (image tag defaults to the chart's appVersion,
so it is released with mail-mcp), a 1 Mi `emptyDir` for the socket shared with
the main container, a 64 Mi staging `emptyDir` (read-write in mail-mcp,
read-only in the sidecar), and `PDF_EXTRACTOR_SOCKET` and `PDF_STAGE_DIR` on the
main container. The sidecar does not mount the cache volume. Both containers
must run as the same uid (the chart's `podSecurityContext` does this): staged
files are owner-only. The sidecar needs no network access and the pod adds no
network need, so NetworkPolicies already written for mail-mcp (the nexus
deployment has them in people-oleg) need no change.

Without the chart: run `pdftext --socket /run/pdftext/pdftext.sock --root
<stage dir>` with the stage directory mounted read-only, and start mail-mcp with
`--pdf-extractor-socket /run/pdftext/pdftext.sock --pdf-stage-dir <stage dir>`. The image is built from
`cmd/pdftext/Dockerfile` (build context: the repository root).

Licence of the image: mail-mcp is MIT, but the image also contains poppler
(`pdftotext`), which is GPL-2.0-or-later, so the image is labelled
`MIT AND GPL-2.0-or-later`. The poppler source for the installed version is
available from Alpine's package page for `poppler-utils`
(<https://pkgs.alpinelinux.org/package/v3.22/main/x86_64/poppler-utils>) and
from <https://poppler.freedesktop.org/>.

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
