# upfile API

The Go service has separate public and administrator listeners. All data is JSON
unless otherwise specified. Errors are `{ "error": "actionable message", "code": "stable_code" }`.
Mutations require the exact configured `Origin` and `X-Upfile-Request: 1` header
(tus PATCH uses the same header). No cross-origin access is allowed. Fetch uses
same-origin credentials. Admin APIs require a validated Cloudflare Access JWT.

Times are Unix seconds. Sizes are integer bytes (at most Number.MAX_SAFE_INTEGER).
All administrator size inputs (global maximum file size, total storage budget,
new/edit request maximum file size, and create/edit upload link maximum file size) use
decimal megabytes (1 MB = 1,000,000 bytes) in the UI and convert exactly to bytes
for this API; the API and persisted units are unchanged. Blank request/link
overrides still inherit the parent maximum.
IDs are opaque lowercase hex. Nullable size overrides are `null` to inherit.
Collection endpoints return `{items: [...], total: number}` and accept `q`, `page`
(one-based), and `limit` (1–100, default 25).

## Administrator listener

- `GET /api/settings` → `{configured, max_file_bytes, storage_budget_bytes,
  default_link_hours, stored_bytes, reserved_bytes, chunk_bytes, lease_seconds,
  max_records, record_count, cleanup_errors}`. Default lifetime is 168 hours.
- `PUT /api/settings` with `{max_file_bytes, storage_budget_bytes, default_link_hours}`.
- `GET /api/containers` → paginated containers:
  `{id,name,instructions,max_file_bytes,created_at,last_activity,status,file_count,stored_bytes,
  active_uploads,active_downloads,link_count}`.
- `POST /api/containers` with `{name,instructions,max_file_bytes}` → container plus
  `initial_link` (a link object including its one-time-visible `url`). Creates both
  records and their audit events atomically. The initial link is labeled
  `Default link`, inherits the request limit (`max_file_bytes: null`), and expires
  after `default_link_hours`.
  The UI immediately presents its URL for copying; no second creation step is needed.
  Subsequent reads never return the secret URL. Editing a container does not create a link.
- `GET /api/containers/:id` → container, plus `effective_max_bytes`.
- `PUT /api/containers/:id` with `{name,instructions,max_file_bytes}` → container.
- `DELETE /api/containers/:id` → `{status:"deleted"|"deleting"}`. UI confirms
  container name and affected files/links/active uploads/downloads first.
- `GET /api/containers/:id/links` → paginated links:
  `{id,container_id,sender_label,expires_at,max_file_bytes,effective_max_bytes,
  status,created_at,file_count,active_uploads}`. Status active/expired/revoked.
- `POST /api/containers/:id/links` with `{sender_label,expires_at,max_file_bytes}`
  → link plus `url` (contains fragment secret, shown only now).
- `PUT /api/links/:id` with `{sender_label,expires_at,max_file_bytes}` → link.
- `POST /api/links/:id/revoke` with `{}` → `{status:"revoked"}`.
- `POST /api/links/:id/rotate` with `{}` → link plus new `url`; old credentials
  and unfinished attempts become invalid. Existing received files remain.
- `GET /api/containers/:id/files` → paginated files:
  `{id,container_id,link_id,name,original_name,sender_label,comment,size,created_at,status}`.
  Status ready/deleting.
- `PUT /api/files/:id` with `{name}` → file.
- `DELETE /api/files/:id` → `{status:"deleted"|"deleting"}`.
- `GET /api/files/:id/download` → attachment; supports Range.
- `GET /api/audit` → paginated `{id,actor,action,target,created_at}` records.

Revocation and rotation atomically record their audit event, invalidate sessions,
and cancel unfinished attempts. If that transaction fails, existing credentials
and attempts remain unchanged. After a successful commit, storage cleanup errors
are logged and retried; rotation still returns its one-time replacement URL.
Pending cleanup remains visible in `cleanup_errors`, and reserved bytes stay
charged until cleanup succeeds.

Container and link creation prepare their complete responses before committing;
failed audit or response preparation rolls back all new records. Once committed,
no database reload or unrelated upload cleanup can hide the one-time URL.
Container/link edits atomically audit the change and cancel affected uploads that
no longer meet the limits or have invalid credentials; cleanup is retried after commit.

File and container deletion atomically record the audit event with the deletion
intent; container deletion also revokes its links, removes sessions, and cancels
unfinished uploads in that transaction. Failed audit leaves the original state,
uploads, downloads, and stored bytes intact. Only after commit are transfers stopped
and bytes removed. Cleanup or state-confirmation failures are logged and retried,
not returned as a failed mutation. The response remains `{status:"deleting"}` until
deletion is confirmed, then becomes `{status:"deleted"}`. Pending deletion retries
do not emit duplicate deletion events. Stored and reserved bytes remain charged
until physical cleanup is confirmed; accepted deletion is never rolled back.

## Public listener

Uploader page is `/u/:linkID#secret`. Capture and immediately remove the fragment;
exchange it once. A reload without fragment uses the current link-specific cookie.
Never persist secrets in localStorage. Do not put filename/comment into tus metadata.

- `POST /api/links/:id/exchange` with `{secret}` → public link information and
  a Secure HttpOnly link-specific cookie. Reuses a valid existing cookie.
  After validating the link and secret, exchanges are limited to 120 per minute
  per link; excess requests return `429` (`rate_limited`). Invalid or unavailable
  links and incorrect secrets do not consume or create rate-limit buckets.
- `GET /api/links/:id` → `{id,title,instructions,max_file_bytes,expires_at,
  chunk_bytes,session_expires_at,busy,reset_available}`; requires cookie.
- `POST /api/links/:id/attempts` with `{key,name,comment,size}` → attempt
  `{id,status,size,offset,upload_url,created_at}`. The UUID key is generated
  before the first request and reused across retries. Identical retry returns
  same attempt; changed payload is `idempotency_conflict`. Status
  uploading/finalizing/completed/canceled/abandoned. Absolute upload_url is
  always on the configured public origin.
- `GET /api/links/:id/attempts/:attemptID` → owned attempt/receipt.
- `POST /api/links/:id/attempts/:attemptID/cancel` with `{}` → `{status:"canceled"}`.
- `POST /api/links/:id/reset` with `{}` → `{status:"reset"}`. Cancels only a stale
  lease. Does not disclose or transfer ownership of another session's upload.
- `HEAD|PATCH /api/links/:id/uploads/:attemptID` → tus 1.0.0 protocol.
  No public tus POST, GET, DELETE, deferred length, concatenation, or overrides.
  HEAD after completion reports full offset, permitting a lost-response retry.

Use tus-js-client with `uploadUrl` from admission, `chunkSize` from link info,
`headers: {'X-Upfile-Request':'1'}`, `withCredentials:true`,
`storeFingerprintForResuming:false`, no endpoint/metadata/parallel uploads.
Keep the browser timeout at 60 seconds for HEAD, but disabled for PATCH;
the server enforces a rolling one-minute read inactivity deadline rather than a
maximum chunk duration, so slow, progressing transfers are not aborted.
Guard `onShouldRetry`: retry network/408/429/5xx and offset conflicts with bounded
backoff for up to three consecutive retries; successful upload progress resets
the tus retry budget. Never retry authorization, revoked/expired, lost-resource,
size, or quota errors. A client fallback to POST cannot allocate an upload. After tus success,
fetch the attempt receipt; only `completed` is success.

Initial UI queue is serial. Every file has a comment field (2048 UTF-8 bytes),
progress, remove/cancel, and result. Use one admission key per intentional upload.
Retry nonterminal admission errors with the same key; explicit fresh starts use a
new key. Preserve succeeded items, continue file-specific errors, stop queue for
link/session/quota/busy errors. Show per-file and byte-weighted total progress,
counts and mixed results. Reset stale attempts only after user confirmation.

Important public error codes include `session_required`, `link_unavailable`,
`busy`, `storage_full`, `record_limit`, `too_large`, `invalid_input`,
`idempotency_conflict`, `attempt_unavailable`, `rate_limited`, and `internal`.
On session_required ask the sender to reopen the original link. Link info never
contains sender labels, other uploads, remaining global capacity or admin data.
