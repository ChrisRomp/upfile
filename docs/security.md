# Security model and limitations

## Deployment trust boundary

The supported deployment has separate public and administrator HTTP listeners,
reachable only through a configured Cloudflare Tunnel. Production Compose
publishes no host ports. The public listener does not expose admin APIs or file
downloads; the admin listener protects pages, APIs, and downloads with validated
Cloudflare Access JWTs.

Cloudflare Access must protect the **entire admin hostname**, with an explicit
allow policy for approved account members/identities and enforced MFA. A UI-only
policy is insufficient. The application validates JWT signatures, algorithm,
issuer, audience and temporal claims; it does not trust an email header as proof
of identity. Keep issuer/audience configuration and outbound certificate fetches
working. Missing or unverifiable authentication fails closed.

All administrators have equivalent rights. There are no per-container roles,
local passwords, or release authentication-bypass switches. Development uses a
separate loopback fixture with freshly generated signing material and the real
JWT verification path; never expose it externally or put its credentials in a
production deployment.

Trust the host, Docker administrators, the application image, and Cloudflare.
The private bridge does not protect against host root or a compromised Docker
daemon. Only the connector belongs on the app's origin network. Forwarded
identity/IP headers from an arbitrary proxy or client are not a trust boundary.

## Upload links are bearer credentials

The secret in an upload URL's fragment authorizes submissions to one link and
its fixed container. A fragment is not sent in the initial HTTP request; the
page removes it and exchanges it through a same-origin request for a bounded,
Secure, HttpOnly, link-specific session cookie. Secrets must not be logged or
persisted in browser localStorage. The application stores credential hashes,
not recoverable link secrets.

This reduces accidental URL/referrer leakage, not theft from browsers,
extensions, clipboards, screenshots, messaging systems or compromised devices.
Treat the **full link** as sensitive. A stripped URL copied after exchange is
not a replacement for the original link. When the cookie expires, the sender
must reopen the original full URL.

An administrator-assigned sender label is **not verified sender identity**.
Anyone with a forwarded or stolen link can repeatedly submit files until expiry
or revocation, potentially exhausting the remaining budget. Rate limits,
per-file ceilings, record limits, reservations, and concurrency limits reduce
resource abuse but do not identify the person uploading. Use short expirations
where appropriate and revoke or rotate a leaked link promptly.

Sessions cannot browse previous submissions or take over another session's
active file. A stale-attempt reset discards the abandoned partial upload; it
does not transfer access to it. Live-page retries are supported, not automatic
cross-device or browser-restart resumption.

Link revocation/rotation and container deletion remove upload authorization.
Revoking or expiring a link does not delete previously received files.

## Untrusted files and browser boundaries

Uploads are inert bytes stored outside the static web root under opaque
server-generated names. Original names, display names and comments are
untrusted metadata, not filesystem paths or HTML. The service does not execute
uploads, unpack archives, parse documents, generate previews, or run malware
scans.

Only authenticated administrators can download files. Downloads are attachments,
not inline previews. These restrictions reduce browser execution risks but do
**not** make a downloaded file safe to open. Scan files in an appropriate
security environment and treat executables, documents, archives and links inside
them as untrusted.

Same-origin mutation checks, required request headers and secure cookies protect
browser request boundaries. Do not add permissive CORS, shared-domain upload
cookies, third-party scripts, injected analytics, or proxy cache rules that
weaken those boundaries. The UI has no third-party analytics, scripts or fonts.

## Data and availability

Browser TLS ends at Cloudflare. Cloudflare can inspect uploaded and downloaded
content; the host operator can read stored bytes. This is **not end-to-end
encryption** and account-free upload is not anonymity from either operator.
The connector-to-app hop is HTTP on the private Docker bridge. Encrypt host
storage and backups separately if required by your data policy.

Byte budgets do not reserve host disk against other applications. Keep physical
headroom and monitor metadata, logs, database size and failed cleanup as well as
uploaded bytes. A malicious link holder or network attacker may still cause
denial of service despite application limits. Cloudflare/WAF policies must be
tested without breaking valid chunked requests.

SQLite metadata and file bytes form one persistent unit. Use one local volume
and one app replica. Network filesystems, shared-volume replicas and horizontal
scaling are unsupported. Back up the whole stopped volume, protect archives as
sensitive, and test recovery.

Completed files remain until explicitly deleted; upload housekeeping retention
is not a completed-file retention policy. Deletion is permanent in the UI, but
there is no secure-erasure guarantee, and independent backups can retain deleted
data. A restored backup can also restore links or sessions revoked after the
snapshot: review them before reconnecting it to the public tunnel.

## Operator response

- **Leaked link:** revoke or rotate it, investigate unexpected submissions, and
  communicate a new full URL through a trusted channel. Received files remain.
- **Compromised administrator:** remove its Access authorization, revoke its
  sessions through Cloudflare, review account/MFA configuration and audit events,
  and consider links or files it could have accessed.
- **Leaked tunnel token:** rotate it and terminate untrusted connectors in
  Cloudflare, replace the host secret, and recreate `cloudflared`. Merely editing
  a local file does not revoke a stolen credential.
- **Host compromise:** stop public access, rotate affected credentials, rebuild
  on a trusted host, and restore only reviewed backups. Container hardening
  cannot make a compromised host trustworthy.
- **Suspicious upload:** do not open it on an ordinary workstation. Follow your
  organization's malware handling and evidence-retention procedure.

See [deployment operations](deployment.md) for concrete configuration,
credential permissions, network requirements, and backup/restore steps.
