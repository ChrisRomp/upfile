import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { checkLockfile } from './check-lockfile-integrity.mjs';

const integrity = `sha512-${createHash('sha512').update('fixture').digest('base64')}`;
const resolved = 'https://registry.npmjs.org/@test/package/-/package-1.0.0.tgz';
const pkg = { version: '1.0.0', integrity, resolved };
const metadata = { name: '@test/package', version: '1.0.0', dist: { integrity, tarball: resolved } };
const lock = value => ({ packages: { '': { name: 'root' }, 'node_modules/@test/package': value } });

test('checks exact scoped package metadata without modifying the lock', async () => {
  const value = lock({ ...pkg });
  const before = structuredClone(value);
  assert.equal(await checkLockfile(value, async (url, options) => {
    assert.equal(url, 'https://registry.npmjs.org/%40test%2Fpackage/1.0.0');
    assert.ok(options.signal instanceof AbortSignal);
    return Response.json(metadata);
  }), 1);
  assert.deepEqual(value, before);
});

test('rejects weak integrity and nonpublic tarballs before fetching metadata', async () => {
  for (const value of [
    { ...pkg, integrity: 'sha1-placeholder' },
    { ...pkg, resolved: resolved.replace('registry.npmjs.org', 'example.invalid') },
  ]) {
    await assert.rejects(checkLockfile(lock(value), async () => assert.fail('must not fetch')));
  }
});

test('fails on mismatched metadata, missing hashes, and network errors', async () => {
  for (const value of [
    { ...metadata, version: '2.0.0' },
    { ...metadata, name: '@other/package' },
    { ...metadata, dist: { ...metadata.dist, integrity: `${integrity.slice(0, -3)}A==` } },
    { ...metadata, dist: { tarball: resolved } },
    { ...metadata, dist: { ...metadata.dist, tarball: 'https://example.invalid/file.tgz' } },
  ]) {
    await assert.rejects(checkLockfile(lock(pkg), async () => Response.json(value)), /differs/);
  }
  await assert.rejects(checkLockfile(lock(pkg), async () => new Response('', { status: 503 })), /HTTP 503/);
  await assert.rejects(checkLockfile(lock(pkg), async () => { throw new Error('offline'); }), /offline/);
});
