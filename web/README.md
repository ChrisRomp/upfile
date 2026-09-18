# upfile web

React and TypeScript interfaces for the administrator and public upload listeners.
No third-party runtime assets, analytics, file previews, or persisted upload credentials.

## Build

```sh
cd web
npm ci
npm test
npm run build
```

The production files are in `web/dist/`. Embed and serve this directory from both
listeners. Serve `/bootstrap.js` unchanged as JavaScript: it must run synchronously
before the application module, capturing the fragment in memory and immediately
removing it from the address. The built page requires no inline-script CSP exception.

The admin listener returns `index.html` for `/`, `/containers/:id`, `/settings`,
and `/audit`; the public listener serves the app under `/u/:linkID`, not `/`.
API routes must not fall back to HTML. `GET /api/surface` selects the authorized
listener surface; public listeners never initialize administrator data requests.
Branding links home only on a known admin surface. It is non-interactive on
public, loading, and unknown/error surfaces so it cannot discard an upload queue
or navigate to an unavailable public root.

## API integration

`../docs/api.md` is the contract. All JSON calls use same-origin credentials.
Mutations use JSON and `X-Upfile-Request: 1`. Dates are Unix seconds; limits and
comments are validated in bytes. The UI treats limits as advisory and surfaces
server-side changes and errors.

The tus client is pinned to 4.3.1. It receives only an admitted upload URL, with
no creation endpoint or metadata. An additional method/URL guard permits only
HEAD and PATCH on that exact same-origin resource. Because this client version
does not apply a top-level `withCredentials` option itself, the request hook also
sets `XMLHttpRequest.withCredentials = true`. Transfers allow three consecutive
retries, with the displayed count taken from tus and its configured delay array.
Successful upload progress resets that retry budget. Retries retain the same
admission key and never allocate through tus POST. Cancellation awaits
`abort(false)`, then uses the application cancellation API and checks its receipt.

HEAD requests have a 60-second browser timeout. PATCH requests have no browser
wall-clock timeout: the server refreshes its one-minute read deadline as data
arrives, allowing slow but progressing chunks to finish. Proxy/network timeouts
can still interrupt requests and use the normal resumable retry flow.

The UI confirms success only after a completed receipt (including an already
completed admission response). Native browser downloads avoid buffering large
files in JavaScript; download progress/errors belong to the browser download UI.
The backend must provide attachment headers and an actionable download failure.

Request-wide active transfer counts are shown in file deletion confirmations:
the file model does not expose per-file active download counts. Pending cleanup
is displayed until refreshed; aggregate cleanup errors appear in Settings.

Request sections use manually activated ARIA tabs: Left/Right wrap focus,
Home/End focus the first/last tab, and Enter/Space selects it. Only one tab is
in the tab order; leaving and re-entering the tab list returns to the selected
tab. Moving focus alone does not fetch or replace a section.

## Tests

`npm test` covers admission idempotency, serial order, completed receipts,
cancellation ordering, retry classification, capacity pauses, UTF-8 limits,
same-origin headers, fragment removal, and duplicate exchange initialization.

`src/tus.integration.test.ts` also runs the actual pinned tus client against an
ephemeral local HTTP server, without mocking the client or transport. It proves
existing-resource HEAD/PATCH-only transfers, Content-Length/chunk bounds, nonzero
offset recovery, lost chunk and final responses, completed-resource recovery,
offset conflicts, terminal HEAD failures, and bounded transient retries. The
server rejects creation and all other methods; no creation endpoint is supplied.

`tests/browser_smoke.py` uses Python Playwright and local Chrome against a Vite
preview at `http://127.0.0.1:4178`, mocking only same-origin API responses. It
exercises admin setup/CRUD/link replacement/download requests, safe text rendering,
mobile uploads through the actual tus client, lost-admission retries, cancellation,
404 handling without creation fallback, repeated use, and session expiry.

```sh
npm exec vite -- preview --host 127.0.0.1 --port 4178 --strictPort
# In another terminal with Python Playwright installed:
python tests/browser_smoke.py
```

Browser screenshots go to ignored `test-results/`. No production credentials or
deployed service are needed.
