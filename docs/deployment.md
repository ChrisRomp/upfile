# Deployment and operations

## 1. Prepare the host

Use Docker Engine with a current Docker Compose plugin and a persistent **local**
filesystem. The supported topology is one app process and one local data volume:
no NFS, SMB, shared SQLite volume, or horizontally scaled app replicas. Reserve
space for uploads, partial uploads, SQLite, logs, backups, and other host users.
Set the application's storage budget below physical capacity.

The image builds the interface with Node.js 24 and the static Go executable with
Go 1.27, then runs in `distroless/static-debian12:nonroot` with CA certificates.
There is no shell, Node runtime, or compiler in the application image. Both
containers drop all capabilities, forbid privilege escalation, use read-only
root filesystems, and have memory/PID limits. The app gets a writable `/data`
volume and a bounded, non-executable scratch mount at `/run/upfile`.

The dedicated bridge has no published host ports. It is deliberately **not**
marked `internal: true`, because both containers need outbound connectivity:

- `cloudflared`: DNS and Cloudflare Tunnel port 7844 over UDP (QUIC) or TCP
  (HTTP/2). Follow the current [Cloudflare firewall destinations][firewall].
- `app`: DNS and outbound HTTPS/443 to the configured
  `<team>.cloudflareaccess.com` issuer for signing certificates. Blocking this
  prevents initial authentication and later key refreshes.
- Building/pulling images additionally requires HTTPS to the image and package
  registries. Keep the host clock synchronized for JWT expiration checks.

Do not add host port mappings, host networking, unrelated services on this
bridge, or Docker-socket mounts. The bridge is not isolation from host root or
other users with Docker administration rights.

### Build networking

The committed lockfiles use canonical `registry.npmjs.org` tarball URLs. Default
Compose builds and CI use public npm without a private npm configuration file
or Microsoft feed access:

```sh
docker compose up --build -d
```

Host `npm` and `make` commands retain the machine's existing npm configuration.
npm treats the default-registry URLs in these lockfiles as portable and uses
the configured registry when it differs. Do not replace them with
machine-specific mirror URLs when updating dependencies.

Docker does not inherit the host's npm userconfig. On a machine that requires
a registry/proxy, opt in to the additional Compose file and pass the existing
userconfig as a build-only secret:

```sh
UPFILE_NPM_CONFIG_FILE="$(npm config get userconfig)" \
  docker compose -f compose.yaml -f compose.npm-config.yaml config --quiet
UPFILE_NPM_CONFIG_FILE="$(npm config get userconfig)" \
  docker compose -f compose.yaml -f compose.npm-config.yaml up --build -d
```

The override defaults to `${HOME}/.npmrc` if `UPFILE_NPM_CONFIG_FILE` is unset;
the selected file must exist. Keep it **outside the repository**, with
restrictive permissions such as `0600`. The variable contains a **path only**,
never secret contents. Do not put credentials in `.env`, Dockerfile `ARG`/`ENV`,
checked-in files, or command-line URLs. Do not omit this override or use an empty
file to bypass a machine's required package source.

A registry mirror configured by `registry=...` is not necessarily an HTTP
transport proxy; do not invent `proxy` or `https-proxy` settings for it. Preserve
TLS verification and arrange any required corporate CA trust instead of setting
`strict-ssl=false`.

The config is mounted only for the install step; npm's cache/log directory is
temporary memory-backed storage. Neither is copied into an image layer, and
neither runtime service receives the build secret.

For a direct Docker build, explicitly provide the same userconfig:

```sh
docker build --secret "id=npmrc,src=$(npm config get userconfig)" -t upfile:local .
```

A secret's contents do not invalidate the Docker build cache. After changing
proxy configuration, add `--no-cache` to the proxy-aware build command when a
fresh install must be verified. Image pulls and Go module downloads use their
own host/build network configuration; the npm secret does not configure them.

## 2. Configure Cloudflare Access before publishing admin routes

1. Create or select a Zero Trust organization. Note its team domain, such as
   `https://your-team.cloudflareaccess.com`.
2. Under **Integrations → Identity providers**, select **Cloudflare**. Enable
   **Restrict to account members**. Cloudflare documents this supported
   [account-login identity provider][cloudflare-idp]; a third-party IdP is not
   required.
3. Create a self-hosted Access application for `admin.example.com`. Leave its
   path unrestricted so it covers **the entire hostname**, not just `/` or UI
   pages. All `/api/*` paths and `/api/files/:id/download` must be protected.
   Review overlapping/path-specific Access applications so none bypasses it.
4. Use an explicit **Allow** policy for approved administrators. The
   **Cloudflare Account Member** selector can restrict membership to the
   intended account. If not every member should administer upfile, also require
   the approved identities/group; do not accidentally OR a broad membership rule
   with a narrower identity rule. Do not use Everyone, Bypass, or a public
   download exception. Every approved administrator has the same rights.
5. Enforce MFA. Require MFA for the approved Cloudflare accounts and configure
   [Access independent MFA][mfa] for this application/policy. Enable it at the
   organization level first and check that no policy override disables it.
   Do not assume the IdP “Authentication method = mfa” selector works with
   Cloudflare account login: Cloudflare documents that method only for selected
   other IdPs. Verify the actual login prompts with a fresh browser session.
6. Copy this Access application's **Application Audience (AUD)** into
   `UPFILE_AUTH_AUDIENCE`. The issuer is the team domain, not the admin hostname.
   Choose an Access session duration appropriate for your administrators.

The app independently verifies the Access JWT signature, issuer, audience and
time claims. An email header alone is never authentication. There is no local
password fallback or production bypass when Access is unavailable.

Do not put the uploader hostname behind this admin Access application. Uploaders
use their scoped bearer links without a Cloudflare account.

## 3. Create a remotely managed tunnel

Create a Cloudflare Tunnel in the dashboard. Configure its published applications
and final fallback as follows:

| Hostname | Origin service |
| --- | --- |
| `drop.example.com` | `http://app:8080` |
| `admin.example.com` | `http://app:8081` |
| Unmatched host/path catch-all | `http_status:404` |

Use exact hostnames, all paths on each hostname, and no wildcard route to the app.
Confirm that the remote configuration ends in the 404 catch-all. Configure DNS
records for these tunnel hostnames in Cloudflare. This Compose deployment uses
the **remote** configuration; it does not read a local ingress YAML file or
provision DNS, Access, or tunnels.

Keep the original Host header and do not add an HTTP Host override. The app
expects its configured public/admin origins and same-origin browser requests.
TLS terminates at Cloudflare; the encrypted tunnel reaches `cloudflared`, which
uses plain HTTP to the app on the private bridge. Do not enter `https://app:8080`
or disable origin TLS verification to compensate for an incorrect service URL.

The pinned `cloudflare/cloudflared:2026.9.1` image supports `--token-file`.
Its [official release][cloudflared-release] and container tag were checked during
packaging. Review and deliberately update this pin for security fixes; automatic
in-container updating is disabled.

### Store the connector credential

Copy `.env.example` to `.env`. Set the exact, distinct HTTPS origins using
lowercase hostnames, with no path, trailing slash, query/fragment marker, or
explicit default `:443` port. Use canonical IP spellings and punycode for
international domain names; explicit non-default ports such as `:8443` are
supported, but port `:0` is not a valid advertised origin. Set the issuer and
audience separately. The audience is an identifier, **not** the tunnel token.

```sh
cp .env.example .env
mkdir -p deploy
chmod 0700 deploy
```

Use a trusted local editor or secret manager to create `deploy/tunnel-token`
containing only the tunnel's token. Do not run the dashboard's command with the
token on the command line, and do not paste it into shell history or logs.
Then set:

```sh
chmod 0444 deploy/tunnel-token
```

The private `0700` parent directory protects the host file from other ordinary
users. The readable file mode allows the non-root connector (UID/GID 65532) to
read the bind-mounted Compose secret. Local Compose file secrets preserve host
permissions; their `uid`, `gid`, and `mode` options cannot be relied upon to
remap these. The secret is mounted **only into cloudflared**, never the app.
Alternatively, on Linux arrange host ownership/mode or ACLs to give only UID
65532 read access, accounting for rootless/user-namespace mappings.

`.env`, the token, generated keys, data, and build artifacts are excluded from
source control or the Docker build context. `deploy/.gitignore` covers the
default token path. Keep any custom `UPFILE_TUNNEL_TOKEN_FILE` outside source
control and protect its parent directory too. Compose secrets are not encrypted
storage; protect the host and backups.

## 4. Start and configure storage

```sh
docker compose config --quiet
docker compose up --build -d
docker compose ps
docker compose logs --tail=100 app cloudflared
docker compose exec app /upfile healthcheck
```

Configuration validation does not prove that a token is valid, image builds
succeed, or Cloudflare routing works. Check the tunnel's connector health in the
dashboard, then test both hostnames.

The public `/healthz` endpoint and `upfile healthcheck` support local process
readiness. Compose waits for app health before starting the connector. A new
installation must become healthy before first-run settings are saved, otherwise
the tunnel could never reach setup. Health is not proof that Access, DNS,
Cloudflare policies, available storage, or end-to-end uploads work. Monitor those
separately. Docker's health status alone does not restart an unhealthy process;
the restart policy applies when a container exits.

Open the admin hostname and sign in. Set the global maximum file size and total
storage budget in first-run setup; these have **no baked-in deployment default**.
Choose a budget that leaves physical headroom. A file must fit the strictest of
global, container, and link limits. The budget counts stored bytes and active
reservations. Reducing limits does not delete completed files.

### Data ownership

The runtime UID/GID is **65532:65532**. The image supplies `/data` owned by that
user with mode `0700`; a fresh, ordinary Docker named volume inherits this
directory's ownership on first use. The app stores `metadata.db` and its
SQLite sidecar files there, with `partial/` and `files/` subdirectories for
unfinished and completed uploads.

Existing volumes and bind mounts do not have their permissions repaired
automatically. Provision a local bind-mount directory for UID/GID 65532 with
mode `0700` before startup. If a restored volume is incorrectly owned, stop the
app and correct ownership with the maintenance command in the restore section.
Do not solve permission errors by running the app as root or making data
world-writable. Rootless Docker may require translated host IDs; verify them
against your Docker configuration.

### Environment reference

| Variable | Default | Meaning |
| --- | --- | --- |
| `UPFILE_PUBLIC_ORIGIN` | required | Exact public HTTPS origin |
| `UPFILE_ADMIN_ORIGIN` | required | Separate exact admin HTTPS origin |
| `UPFILE_AUTH_ISSUER` | required | Access team HTTPS issuer |
| `UPFILE_AUTH_AUDIENCE` | required | Admin Access application's AUD |
| `UPFILE_DATA_DIR` | `/data` | Persistent state root |
| `UPFILE_PUBLIC_ADDR` | `:8080` | Public HTTP bind address |
| `UPFILE_ADMIN_ADDR` | `:8081` | Admin HTTP bind address |
| `UPFILE_CHUNK_BYTES` | `16777216` | Maximum chunk size, 16 MiB |
| `UPFILE_MAX_ACTIVE` | `8` | Aggregate active upload limit |
| `UPFILE_MAX_RECORDS` | `100000` | Metadata/file record ceiling |
| `UPFILE_HEADROOM_BYTES` | `268435456` | Physical free-space reserve, 256 MiB |
| `UPFILE_LEASE_SECONDS` | `300` | Upload activity lease |
| `UPFILE_SESSION_SECONDS` | `86400` | Maximum uploader session lifetime |
| `UPFILE_RETENTION_SECONDS` | `86400` | Inactivity before abandoned partial-upload cleanup; **not completed-file expiry** |
| `UPFILE_NPM_CONFIG_FILE` | `${HOME}/.npmrc` | Used only with `compose.npm-config.yaml`: private npm userconfig path for the build secret |
| `UPFILE_TUNNEL_TOKEN_FILE` | `./deploy/tunnel-token` | Compose-only host secret path |

Compose fixes the data path and listener addresses to match its volume and
tunnel routes. Change those only with a coordinated deployment change. Resource
values are integer bytes/counts or seconds; invalid settings fail startup.
Session lifetime is also bounded by the link's expiry. The admin UI controls
maximum file size, budget, and the default lifetime for **new** links (initially
seven days); these persist in the database.

## Cloudflare request settings

- Bypass cache on both application hostnames, including APIs, upload responses,
  and downloads. Do not enable Cache Everything, HTML rewriting, injected
  analytics, or script optimizers for the application.
- Keep chunks below the effective Cloudflare/zone per-request limit. The default
  16 MiB is below the documented Free/Pro 100 MB limit, but a zone may have a
  lower limit. Check the current [upload-limit documentation][upload-limits].
- Ensure scoped WAF/rate-limit rules permit legitimate JSON admission/exchange
  and tus `HEAD`/`PATCH` traffic. Browser challenges in the middle of a chunk
  stream break retries; test rules on `/api/links/*`. Do not broadly disable
  security for the whole zone.
- Chunking avoids single-request size ceilings, not Cloudflare bandwidth/service
  terms, plan constraints, or download timeouts. Review your agreement before
  using this for large file delivery. Test slow chunks and authenticated Range
  downloads on your actual plan.

## Routine operations

Watch host free space, stored/reserved usage, pending cleanup errors, container
health and memory, and tunnel connectivity. Logs are locally rotated to three
10 MB files per service. Protect them and audit records; do not enable raw
request/header/body logging, which may disclose credentials or submitted data.

If admission is blocked, inspect the budget, per-file limits, record ceiling,
active slots, and real disk capacity in that order. Pending physical deletion
does not necessarily free quota immediately. Resolve storage/permission errors
and allow cleanup to complete; never delete arbitrary database or partial files
by hand while the app is running.

Revoking or rotating a leaked link stops further use; received files remain.
Completed files persist until an administrator explicitly deletes them. There
is no trash, secure-erasure promise, or automatic completed-file expiration.

For upgrades, record the current source revision and image identities, take a
stopped-app backup, update deliberately, and run the smoke checks again. Do not
assume a previous binary can open a migrated database; rollback restores the
matching image **and** its full backup. `docker compose down` retains the named
volume; **`docker compose down -v` destroys it**.

Rotate a compromised tunnel token in Cloudflare, replace the local secret
(temporarily restore owner write permission if needed), and recreate the
connector with `docker compose up -d --force-recreate cloudflared`. Terminate
untrusted connector sessions in Cloudflare as directed by its
[token-rotation guidance][tunnel-token]; local replacement alone is insufficient.
This single-connector deployment may have a brief interruption.

## Backup and restore

Back up the **whole `/data` tree while the app is stopped**, including SQLite,
any WAL/journal files, partial uploads and completed files. Copying only a live
database, or taking separate live snapshots of metadata and bytes, is not a
consistent backup. Coordinate a maintenance window: stopping can interrupt
uploads/downloads. Keep configuration and image/source versions with the backup
in a protected location; keep tunnel credentials in a separate secret store.

The commands below use a short-lived Alpine maintenance container and stream
the archive through stdout/stdin. The runtime remains non-root. Run from the
repository directory with the normal `.env` available:

```sh
mkdir -p deploy/backups
chmod 0700 deploy/backups
docker compose stop cloudflared app
APP_ID=$(docker compose ps -aq app)
test -n "$APP_ID"
BACKUP="deploy/backups/upfile-$(date -u +%Y%m%dT%H%M%SZ).tar.gz"
(umask 077; docker run --rm --network none --read-only \
  --volumes-from "$APP_ID":ro alpine:3.23 \
  tar czf - -C /data . > "$BACKUP")
gzip -t "$BACKUP"
tar tzf "$BACKUP" | head
docker compose start app cloudflared
```

Stop if any command fails; do not label a partial archive a successful backup.
The `test` and archive checks are explicit manual gates. Copy verified archives
to protected, preferably encrypted off-host storage. Ensure enough disk exists
for the archive as well as the application. Periodically test restoration.

Restore into a **fresh empty volume** in an isolated deployment, not over a
running or populated tree. The distinct Compose project name below creates
separate containers, network and volume; choose a new name if that project has
been used before. Do not connect an old backup to the production tunnel while
the production app is still using the same tunnel or receiving uploads. With
the replacement app container created but not started:

```sh
export COMPOSE_PROJECT_NAME=upfile-restore
docker compose create app
APP_ID=$(docker compose ps -aq app)
test -n "$APP_ID"
gzip -t deploy/backups/selected-backup.tar.gz
docker run --rm -i --network none --read-only \
  --volumes-from "$APP_ID":rw alpine:3.23 \
  sh -c 'test -z "$(ls -A /data)" &&
    tar xzf - -C /data &&
    chown -R 65532:65532 /data &&
    chmod 0700 /data' < deploy/backups/selected-backup.tar.gz
docker compose up -d app
docker compose exec app /upfile healthcheck
```

Extract only archives you trust. Reconcile any revocations/deletions made after
the backup before reopening public access. Stop the original connector before
starting the replacement with `docker compose up -d cloudflared`; for a rehearsal,
use a separate test tunnel/hostnames/Access application rather than production
credentials. Verify settings, counts, filenames, completed bytes and download
hashes through the authenticated admin interface before resuming ordinary use.
Retain the pre-restore data until verification is complete, and explicitly
select the intended Compose project for future operations. For ownership repair
of an existing volume, stop the app and use the same maintenance mount with only
`chown -R 65532:65532 /data && chmod 0700 /data`.

## Release verification

Run `make test` and `make build` first. Before calling a release deployment-ready,
also record results for these explicit integration/manual checks:

- Browser flows on desktop and mobile: first-run settings; admin CRUD; file
  selection and drag/drop; comments; serial queue progress; retry/cancel; mixed
  outcomes; link reuse; expiry/revocation; keyboard interaction.
- Stream at least **3 GiB** through the complete local HTTPS stack. Confirm byte
  count and a local SHA-256 hash against the authenticated download, record peak
  app/container memory (`docker stats` when containerized), and verify each
  request stays within the chunk ceiling. With a configured local `make dev`
  fixture running, use `UPFILE_LARGE_TEST=1 node e2e/large-upload.cjs`; set
  `UPFILE_SERVER_PID` to its numeric server PID to also enforce the 256 MiB RSS
  bound. See [test prerequisites and cleanup](../README.md#build-and-test).
  This complements the Go handler test, not the real Cloudflare check. Also
  throttle a progressing transfer to test deadlines and lease handling. Do not
  buffer the test payload in memory.
- Kill/restart during allocation and finalization, verify reconciliation without
  duplicate records, and delete a container during active upload/download.
- Restart with the same volume and verify persisted settings/files. Execute the
  backup/restore procedure in isolation and compare downloaded hashes.
- On a real configured tunnel, verify denied unauthenticated admin API/download
  requests, successful approved-account MFA login, multi-chunk upload, a slow or
  retried chunk, and a reauthenticated Range download. An expired download
  session may require signing in again and explicitly restarting/resuming the
  download; automatic browser resumption is not guaranteed.

These checks require resources beyond unit tests. Report any unrun check as
**pending**, including when a Docker daemon, sufficient storage, or configured
Cloudflare services are unavailable. A successful `docker compose config` is
not a substitute for building and running the image.

[cloudflare-idp]: https://developers.cloudflare.com/cloudflare-one/integrations/identity-providers/cloudflare/
[mfa]: https://developers.cloudflare.com/cloudflare-one/access-controls/policies/mfa-requirements/
[firewall]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-with-firewall/
[upload-limits]: https://developers.cloudflare.com/cache/concepts/default-cache-behavior/#upload-limits
[cloudflared-release]: https://github.com/cloudflare/cloudflared/releases/tag/2026.9.1
[tunnel-token]: https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/remote-tunnel-permissions/
