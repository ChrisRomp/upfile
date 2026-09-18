# upfile

Self-hosted file collection without uploader accounts. Create a request container,
share an expiring upload link, and receive multiple files with optional comments.
Links stay reusable until expired or revoked. Administrators sign in through
Cloudflare Access using their Cloudflare account.

The Go service embeds a React interface and stores SQLite metadata and file bytes
on one local volume. Chunked, serial uploads support multi-gigabyte files without
a multi-gigabyte HTTP request. Container and link size limits can tighten the
global per-file limit; a separate storage budget bounds aggregate usage.

## Deploy

You need Docker Engine with Compose, a local filesystem with enough free space,
and a Cloudflare domain, Zero Trust organization, and remotely managed tunnel.
This repository does not create Cloudflare resources.

1. Follow [deployment setup](docs/deployment.md) to route `drop.example.com` to
   `http://app:8080` and `admin.example.com` to `http://app:8081`, with an unmatched
   route returning 404. Protect **every admin path**, including APIs and downloads,
   with an Access allow policy for approved account members and enforced MFA.
2. Copy `.env.example` to `.env`; set both exact HTTPS origins, the Access team
   issuer, and the admin application's audience. Set `UPFILE_NPM_CONFIG_FILE`
   to your private npm userconfig if it is not at the default `~/.npmrc`,
   as described in [build networking](docs/deployment.md#build-networking).
3. Put the tunnel token in ignored `deploy/tunnel-token`, with the directory and
   file permissions described in the deployment guide. Do not put it in `.env`,
   shell arguments, source control, or an image.
4. Start and check the service:

   ```sh
   docker compose config --quiet
   docker compose up --build -d
   docker compose ps
   docker compose exec app /upfile healthcheck
   ```

5. Open the admin hostname. First-run setup requires you to choose a maximum file
   size and total storage budget in megabytes (MB; 1 MB = 1,000,000 bytes).
   Decimal MB values are supported; existing limits retain their exact byte values.
   All size-entry fields use MB, including new/edit request and create/edit upload
   link limits. Leave an override blank to inherit its parent maximum.
   Creating a container automatically creates a reusable upload link labeled
   **Default link** and immediately shows its full URL for copying. It inherits
   the request's size limit and uses the configured default expiration.
   Additional sender-specific links can still be created within the request.
   The secret is not recoverable later.

Production Compose publishes **no host ports**. Both origin listeners use HTTP
only inside the dedicated Docker bridge; browser TLS ends at Cloudflare. Keep
one app replica and preserve its `/data` volume.

## Local development

Use Node.js 24/npm and Go 1.27 (the Docker build uses these versions). The module
and dependencies determine the minimum Go version; a newer toolchain may be
downloaded automatically by Go.

```sh
make dev
```

This builds the interface and runs the separate `cmd/dev` helper, bound only to
`127.0.0.1`. Open **https://localhost:8444** for administrator first-run setup;
generated upload links use **https://localhost:8443**. Use `localhost` in URLs,
not `127.0.0.1`: mutation requests enforce the exact configured origin.
Development state persists
in ignored `.dev/data`, separately from production, with no preset storage limits.
The helper uses a local issuer and real signed JWT verification, not a release
authentication bypass.

Trust the generated development CA at `.dev/ca.pem` **only for local testing**.
Import it into a dedicated test browser profile's trust store, or explicitly
approve the localhost certificate warning on **both** local HTTPS origins. The
helper makes no OS trust changes. It generates a new CA and signing keys on every
restart, so replace any previous certificate trust/exception after restarting.
Only the public CA certificate is written to disk; private keys remain in memory.
For a command-line health check without disabling TLS verification:

```sh
curl --cacert .dev/ca.pem https://localhost:8443/healthz
```

Never disable TLS verification globally or trust a certificate from someone
else. Remove development trust when finished and keep `.dev` out of source
control. The production image contains only `cmd/upfile`.

## Build and test

```sh
make build       # npm ci, web build, then dist/upfile
make test        # web build, frontend tests, then go test ./...
```

The web build runs first because Go embeds `web/dist`. `GO` and `NPM` may be
overridden, for example `make test GO=/path/to/go`. For the opt-in 3 GiB streaming
check, provide enough free local disk space and run:

```sh
UPFILE_LARGE_TEST=1 go test ./internal/app -run TestLargeStreamingUpload -v
```

For desktop/mobile browser checks, start `make dev` in another terminal, install
the separate browser-test dependencies, and run from the repository root:

```sh
npm ci --prefix e2e
npm test --prefix e2e
```

These tests use installed Google Chrome by default (`PLAYWRIGHT_CHANNEL`
overrides the browser channel). They target only the local HTTPS fixture, not
Cloudflare, and overwrite its storage settings with a **64 MiB per-file limit
and 512 MiB budget**. Use disposable development data; they are not production
smoke tests. The test runner accepts the generated local TLS certificate.

To complement the Go handler test with an actual **3 GiB HTTPS upload and
authenticated download checksum check**, keep `make dev` running, complete its
first-run setup, and run:

```sh
UPFILE_LARGE_TEST=1 node e2e/large-upload.cjs
```

This uses `.dev/ca.pem` to verify TLS and needs at least 3 GiB of free storage
plus headroom. It creates a dedicated test container, then deletes it and
restores previously configured storage settings during cleanup. Check cleanup
after a forcibly interrupted run. Optionally set `UPFILE_SERVER_PID` to the
numeric **server process PID**, not the `go run` launcher, to sample server RSS
and fail above 256 MiB. Without it, integrity is checked but memory is not
measured. This remains a local fixture test, not a Cloudflare smoke test.

Release verification also requires the
[recovery, persistence, and real Cloudflare checks](docs/deployment.md#release-verification);
local tests alone do not establish deployment readiness.

GitHub Actions runs frontend tests/build, Go formatting/vet/race tests,
desktop/mobile browser tests, and a production Docker build on pull requests
targeting `main` and on pushes to `main`. CI uses a fresh local authentication
fixture without Cloudflare credentials. It does not publish images or deploy
services.

## Important boundaries

- A sender label is an administrator's label, **not verified identity**. Anyone
  holding a link can reuse it and potentially fill the available budget.
- Received files are untrusted and **not malware-scanned**. There are no inline
  previews or public downloads. Scan files before opening them.
- Cloudflare and the host operator can see plaintext data. This is not
  end-to-end encryption; encrypt the host disk and backups separately.
- Completed files remain until explicitly deleted. Expiring or revoking a link
  does not remove its files. Back up the **whole stopped `/data` volume**.
- There is no shared-volume/NFS or multi-replica support. Live-page retry is
  supported; browser-restart or cross-device resumption of selected files is not.

See [operations and backup/restore](docs/deployment.md),
[security assumptions](docs/security.md), and the [API contract](docs/api.md).
No third-party analytics, fonts, or scripts are loaded by the application UI.

## License

Copyright (C) 2026 Chris Romp and contributors.

upfile is licensed under the **GNU Affero General Public License, version 3
only** (`AGPL-3.0-only`). See [LICENSE](LICENSE) for the complete terms.
Commercial use is permitted under those terms.

If you modify upfile and let users interact with that modified version over a
network, you must prominently offer those users its corresponding source under
the AGPL, at no charge. For a modified deployment, provide a visible source-code
link to the actual version being run, including the required build/install
sources. Uploaded files, customer data, and deployment secrets are not part of
that source offer. Redistribution also carries the license's source and notice
requirements. Dependencies retain their respective licenses.
