import { appendFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';

function versionParts(value, allowPrefix = false) {
  if (typeof value !== 'string') return null;
  const match = /^(v?)(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.exec(value);
  if (!match || match[0] !== value || (!allowPrefix && match[1])) return null;
  const parts = match.slice(2);
  return parts.join('.').length <= 128 ? parts : null;
}

export function publicationForRef(eventName, ref, deleted = false) {
  if (eventName !== 'push' || deleted) {
    return { publish: false, reason: 'Only non-deleted push events publish images.' };
  }
  if (ref === 'refs/heads/main') return { publish: true, version: 'main' };
  if (ref.startsWith('refs/tags/')) {
    const parts = versionParts(ref.slice('refs/tags/'.length), true);
    if (parts) return { publish: true, version: parts.join('.') };
    return { publish: false, reason: `Unsupported stable release tag: ${ref}` };
  }
  return { publish: false, reason: `Not a publishing ref: ${ref}` };
}

function compareVersions(left, right) {
  for (let index = 0; index < left.length; index++) {
    const a = BigInt(left[index]);
    const b = BigInt(right[index]);
    if (a !== b) return a > b ? 1 : -1;
  }
  return 0;
}

export function selectTags(version, publishedTags) {
  const candidate = versionParts(version);
  if (!candidate) throw new Error(`Invalid stable image version: ${version}`);
  if (!Array.isArray(publishedTags) || !publishedTags.every(tag => typeof tag === 'string')) {
    throw new Error('Published tags must be an array of strings');
  }
  const newer = publishedTags
    .map(tag => versionParts(tag))
    .filter(parts => parts && compareVersions(parts, candidate) > 0);
  const [major, minor] = candidate;
  const tags = [version];
  if (!newer.some(parts => parts[0] === major && parts[1] === minor)) tags.push(`${major}.${minor}`);
  if (!newer.some(parts => parts[0] === major)) tags.push(major);
  if (newer.length === 0) tags.push('latest');
  return tags;
}

export async function fetchPublishedTags(image, username, password, request = fetch) {
  const match = /^ghcr\.io\/([a-z0-9]+(?:[._-][a-z0-9]+)*(?:\/[a-z0-9]+(?:[._-][a-z0-9]+)*)+)$/.exec(image);
  if (!match || match[0] !== image) throw new Error('Expected a lowercase GHCR image name without a tag');
  if (!username || !password) throw new Error('GHCR username and token are required');

  const repository = match[1];
  const registry = new URL('https://ghcr.io');
  const tokenURL = new URL('/token', registry);
  tokenURL.searchParams.set('service', registry.hostname);
  tokenURL.searchParams.set('scope', `repository:${repository}:pull`);
  const options = authorization => ({
    headers: { Authorization: authorization, Accept: 'application/json' },
    signal: AbortSignal.timeout(20_000),
    redirect: 'error',
  });
  const tokenResponse = await request(tokenURL, options(
    `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`,
  ));
  if (!tokenResponse.ok) throw new Error(`GHCR token request: HTTP ${tokenResponse.status}`);
  const credentials = await tokenResponse.json();
  const token = credentials?.token ?? credentials?.access_token;
  if (typeof token !== 'string' || !token) throw new Error('GHCR token response has no bearer token');

  const listURL = new URL(`/v2/${repository}/tags/list?n=100`, registry);
  let pageURL = listURL;
  const visited = new Set();
  const tags = [];
  for (let page = 0; page < 1000; page++) {
    if (visited.has(pageURL.href)) throw new Error('GHCR tag pagination repeated a page');
    visited.add(pageURL.href);
    const response = await request(pageURL, options(`Bearer ${token}`));
    if (!response.ok) {
      if (page === 0 && response.status === 404) {
        const error = await response.json();
        if (Array.isArray(error?.errors) && error.errors.length > 0 &&
            error.errors.every(item => item?.code === 'NAME_UNKNOWN')) return [];
      }
      throw new Error(`GHCR tag listing: HTTP ${response.status}`);
    }
    const body = await response.json();
    if (body?.name !== repository ||
        (body.tags !== null && (!Array.isArray(body.tags) ||
          !body.tags.every(tag => typeof tag === 'string')))) {
      throw new Error('Malformed GHCR tag listing');
    }
    tags.push(...(body.tags ?? []));
    const link = response.headers.get('link');
    if (link === null) return tags;
    const next = /^<([^<>]+)>;\s*rel=(?:"next"|next)$/.exec(link.trim());
    if (!next) throw new Error('Malformed GHCR pagination link');
    pageURL = new URL(next[1], pageURL);
    if (pageURL.origin !== registry.origin || pageURL.pathname !== listURL.pathname ||
        pageURL.username || pageURL.password || pageURL.hash) {
      throw new Error('Unsafe GHCR pagination URL');
    }
  }
  throw new Error('GHCR tag listing exceeded 1000 pages');
}

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} is required`);
  return value;
}

async function main() {
  const output = requiredEnv('GITHUB_OUTPUT');
  switch (process.argv[2]) {
    case 'channel': {
      const publication = publicationForRef(
        requiredEnv('GITHUB_EVENT_NAME'),
        requiredEnv('GITHUB_REF'),
        process.env.EVENT_DELETED === 'true',
      );
      console.log(publication.publish ? `Publishing channel: ${publication.version}` : publication.reason);
      await appendFile(output, `publish=${publication.publish}\nversion=${publication.version ?? ''}\n`);
      break;
    }
    case 'tags': {
      const version = requiredEnv('IMAGE_VERSION');
      let tags = ['main'];
      if (version !== 'main') {
        selectTags(version, []);
        const published = await fetchPublishedTags(
          requiredEnv('IMAGE_NAME'), requiredEnv('GITHUB_ACTOR'), requiredEnv('GHCR_TOKEN'),
        );
        tags = selectTags(version, published);
      }
      console.log(`Publishing image tags: ${tags.join(', ')}`);
      await appendFile(output, `tags<<CONTAINER_TAGS\n${tags.join('\n')}\nCONTAINER_TAGS\n`);
      break;
    }
    default:
      throw new Error('Usage: node scripts/container-tags.mjs channel|tags');
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch(error => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
