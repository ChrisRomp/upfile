import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

const registry = 'https://registry.npmjs.org/';

export async function checkLockfile(lock, fetchMetadata = fetch) {
  const packages = Object.values(lock.packages).filter(pkg => pkg.resolved);
  let index = 0;
  async function worker() {
    while (index < packages.length) {
      const pkg = packages[index++];
      const url = new URL(pkg.resolved);
      const separator = url.pathname.lastIndexOf('/-/');
      if (url.origin !== new URL(registry).origin || separator <= 1 ||
          url.username || url.password || url.search || url.hash) {
        throw new Error(`Expected a canonical public npm tarball URL: ${pkg.resolved}`);
      }
      const name = decodeURIComponent(url.pathname.slice(1, separator));
      if (!/^sha512-[A-Za-z0-9+/]{86}==$/.test(pkg.integrity || '')) {
        throw new Error(`${name}@${pkg.version} does not have SHA-512 integrity`);
      }
      const metadataURL = `${registry}${encodeURIComponent(name)}/${encodeURIComponent(pkg.version)}`;
      const response = await fetchMetadata(metadataURL, { signal: AbortSignal.timeout(20_000) });
      if (!response.ok) throw new Error(`npm metadata for ${name}@${pkg.version}: HTTP ${response.status}`);
      const metadata = await response.json();
      if (metadata.name !== name || metadata.version !== pkg.version ||
          metadata.dist?.integrity !== pkg.integrity || metadata.dist?.tarball !== pkg.resolved) {
        throw new Error(`Lockfile differs from public npm metadata for ${name}@${pkg.version}`);
      }
    }
  }
  // Bound requests while allowing both lockfiles to be checked quickly in CI.
  const results = await Promise.allSettled(Array.from({ length: Math.min(4, packages.length) }, worker));
  const failed = results.find(result => result.status === 'rejected');
  if (failed) throw failed.reason;
  return packages.length;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    for (const file of ['web/package-lock.json', 'e2e/package-lock.json']) {
      const lock = JSON.parse(await readFile(file, 'utf8'));
      const count = await checkLockfile(lock);
      console.log(`${file}: ${count} SHA-512 values verified against public npm metadata`);
    }
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
