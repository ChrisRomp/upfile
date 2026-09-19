const https = require('node:https');
const fs = require('node:fs');
const path = require('node:path');
const { createHash, randomUUID } = require('node:crypto');
const { execFileSync } = require('node:child_process');

// Explicit opt-in: only the isolated cmd/dev localhost fixture is supported.
if (process.env.UPFILE_LARGE_TEST !== '1') throw new Error('Set UPFILE_LARGE_TEST=1 to run the 3 GiB local transfer.');
const ca = fs.readFileSync(path.join(__dirname, '../.dev/ca.pem'));
const admin = 'https://localhost:8443/admin';
const drop = 'https://localhost:8443';
const size = 3 * 1024 ** 3;

async function request(origin, method, url, data, headers = {}) {
  const body = data === undefined ? undefined : Buffer.isBuffer(data) ? data : Buffer.from(JSON.stringify(data));
  return new Promise((resolve, reject) => {
    const req = https.request(new URL(url, origin), {
      ca, method, headers: {
        Origin: origin, 'X-Upfile-Request': '1',
        ...(body ? { 'Content-Type': 'application/json', 'Content-Length': body.length } : {}),
        ...headers,
      },
    }, response => {
      const chunks = [];
      response.on('data', chunk => chunks.push(chunk));
      response.on('error', reject);
      response.on('end', () => {
        const text = Buffer.concat(chunks).toString();
        if (response.statusCode >= 400) return reject(new Error(`${method} ${url}: ${response.statusCode} ${text}`));
        resolve({ status: response.statusCode, headers: response.headers, body: text ? JSON.parse(text) : null });
      });
    });
    req.setTimeout(60000, () => req.destroy(new Error('request timed out')));
    req.on('error', reject);
    req.end(body);
  });
}

async function main() {
  const initial = (await request(admin, 'GET', '/api/settings')).body;
  let container;
  let peakRSS = 0;
  let monitor;
  try {
    // Optional RSS measurement uses this locally launched process only.
    if (process.env.UPFILE_SERVER_PID) {
      const pid = process.env.UPFILE_SERVER_PID;
      if (!/^\d+$/.test(pid)) throw new Error('UPFILE_SERVER_PID must be numeric');
      monitor = setInterval(() => {
        const rss = Number(execFileSync('ps', ['-o', 'rss=', '-p', pid], { encoding: 'utf8' }).trim());
        peakRSS = Math.max(peakRSS, rss * 1024);
      }, 1000);
    }
    await request(admin, 'PUT', '/api/settings', {
      max_file_bytes: Math.max(initial.max_file_bytes, size),
      storage_budget_bytes: Math.max(initial.storage_budget_bytes, initial.stored_bytes + initial.reserved_bytes + size + 1024 ** 3),
      default_link_hours: initial.default_link_hours,
    });
    container = (await request(admin, 'POST', '/api/containers', {
      name: 'Temporary 3 GiB verification', instructions: '', max_file_bytes: null,
    })).body;
    const link = (await request(admin, 'POST', `/api/containers/${container.id}/links`, {
      sender_label: 'Local streaming verification', expires_at: Math.floor(Date.now() / 1000) + 3600, max_file_bytes: null,
    })).body;
    const exchange = await request(drop, 'POST', `/api/links/${link.id}/exchange`, { secret: new URL(link.url).hash.slice(1) });
    const cookie = exchange.headers['set-cookie'][0].split(';')[0];
    const upload = (await request(drop, 'POST', `/api/links/${link.id}/attempts`, {
      key: randomUUID(), name: 'stream.bin', comment: 'Automatically generated test bytes; deleted after verification.', size,
    }, { Cookie: cookie })).body;
    const chunk = Buffer.alloc(exchange.body.chunk_bytes);
    for (let i = 0; i < chunk.length; i++) chunk[i] = i % 251;
    const expected = createHash('sha256');
    const started = Date.now();
    for (let offset = 0; offset < size; offset += chunk.length) {
      const data = chunk.subarray(0, Math.min(chunk.length, size - offset));
      const response = await request(drop, 'PATCH', upload.upload_url, data, {
        Cookie: cookie, 'Content-Type': 'application/offset+octet-stream',
        'Tus-Resumable': '1.0.0', 'Upload-Offset': String(offset),
      });
      if (response.status !== 204) throw new Error('unexpected PATCH response');
      expected.update(data);
    }
    const receipt = (await request(drop, 'GET', `/api/links/${link.id}/attempts/${upload.id}`, undefined, { Cookie: cookie })).body;
    if (receipt.status !== 'completed' || receipt.size !== size) throw new Error('missing durable receipt');
    const actual = createHash('sha256');
    let received = 0;
    await new Promise((resolve, reject) => {
      const req = https.get(`${admin}/api/files/${upload.id}/download`, { ca }, response => {
        if (response.statusCode !== 200) { response.resume(); reject(new Error('download failed')); return; }
        response.on('data', chunk => { actual.update(chunk); received += chunk.length; });
        response.on('end', resolve);
        response.on('error', reject);
      });
      req.on('error', reject);
    });
    const hash = expected.digest('hex');
    if (received !== size || actual.digest('hex') !== hash) throw new Error('download integrity mismatch');
    if (peakRSS > 256 * 1024 ** 2) throw new Error(`server RSS ${peakRSS} exceeds 256 MiB bound`);
    console.log(JSON.stringify({
      bytes: size, chunk_bytes: chunk.length, sha256: hash,
      elapsed_seconds: (Date.now() - started) / 1000,
      peak_server_rss_mib: peakRSS ? +(peakRSS / 1024 ** 2).toFixed(1) : null,
    }, null, 2));
  } finally {
    if (monitor) clearInterval(monitor);
    if (container) await request(admin, 'DELETE', `/api/containers/${container.id}`);
    if (initial.configured) await request(admin, 'PUT', '/api/settings', {
      max_file_bytes: initial.max_file_bytes, storage_budget_bytes: initial.storage_budget_bytes,
      default_link_hours: initial.default_link_hours,
    });
  }
}
main().catch(error => { console.error(error.message); process.exitCode = 1; });
