import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { publicationForRef, selectTags, fetchPublishedTags } from './container-tags.mjs';

test('only main and stable tag pushes publish, with an optional v prefix', () => {
  assert.deepEqual(publicationForRef('push', 'refs/heads/main'), { publish: true, version: 'main' });
  for (const version of ['0.0.0', '0.1.2', '1.0.0', '10.20.30']) {
    for (const prefix of ['', 'v']) {
      assert.deepEqual(publicationForRef('push', `refs/tags/${prefix}${version}`), {
        publish: true, version,
      });
    }
  }
  for (const ref of ['refs/heads/main', 'refs/tags/1.0.0', 'refs/tags/v1.0.0']) {
    for (const event of ['pull_request', 'workflow_dispatch', 'release']) {
      assert.equal(publicationForRef(event, ref).publish, false);
    }
    assert.equal(publicationForRef('push', ref, true).publish, false);
  }
  for (const ref of ['refs/heads/1.0.0', 'refs/heads/testing', 'refs/pull/1/merge']) {
    assert.equal(publicationForRef('push', ref).publish, false);
  }
});

test('unsupported tag formats are explicitly excluded from publishing', () => {
  for (const version of [
    '', '1', '1.0', '1.0.0.0', '01.0.0', '1.01.0', '1.0.01', 'V1.0.0', 'vv1.0.0',
    '1.0.0-rc.1', 'v1.0.0-beta.1', '1.0.0+build.1', '-1.0.0', '1.0.-1',
    ' 1.0.0', '1.0.0 ', '1.0.0\n', 'release/1.0.0', '1.0.0; echo bad',
    `${'1'.repeat(125)}.0.0`,
  ]) {
    const publication = publicationForRef('push', `refs/tags/${version}`);
    assert.equal(publication.publish, false, version);
    assert.match(publication.reason, /Unsupported stable release tag/);
    assert.throws(() => selectTags(version, []), /Invalid stable image version/);
  }
  assert.throws(() => selectTags('v1.0.0', []), /Invalid stable image version/);
});

test('selects exact, minor, major, and latest tags without regressing aliases', () => {
  const cases = [
    ['1.0.0', [], ['1.0.0', '1.0', '1', 'latest']],
    ['1.0.1', ['1.0.0'], ['1.0.1', '1.0', '1', 'latest']],
    ['1.1.0', ['1.0.1'], ['1.1.0', '1.1', '1', 'latest']],
    ['1.0.2', ['1.1.0'], ['1.0.2', '1.0']],
    ['2.0.0', ['1.1.0'], ['2.0.0', '2.0', '2', 'latest']],
    ['1.5.1', ['2.0.0', '1.1.0'], ['1.5.1', '1.5', '1']],
    ['1.0.0', ['1.0.1'], ['1.0.0']],
    ['0.1.0', ['0.0.9'], ['0.1.0', '0.1', '0', 'latest']],
    ['0.0.0', [], ['0.0.0', '0.0', '0', 'latest']],
    ['1.0.0', ['1.0.0'], ['1.0.0', '1.0', '1', 'latest']],
    ['1.0.2', ['1.0.2', '1.1.0'], ['1.0.2', '1.0']],
    ['1.0.0', ['main', 'latest', '1', '1.0', '2.0.0-rc.1', '2.0.0+build.1', 'v2.0.0', '02.0.0'],
      ['1.0.0', '1.0', '1', 'latest']],
  ];
  for (const [version, published, expected] of cases) {
    const before = [...published];
    assert.deepEqual(selectTags(version, published), expected, version);
    assert.deepEqual(published, before);
  }
  assert.throws(() => selectTags('1.0.0', null), /array of strings/);
  assert.throws(() => selectTags('1.0.0', [123]), /array of strings/);
});

test('compares version components numerically without losing integer precision', () => {
  assert.deepEqual(selectTags('1.10.0', ['1.9.0']), ['1.10.0', '1.10', '1', 'latest']);
  assert.deepEqual(selectTags('1.9.0', ['1.10.0']), ['1.9.0', '1.9']);
  assert.deepEqual(selectTags('1.0.10', ['1.0.9']), ['1.0.10', '1.0', '1', 'latest']);
  assert.deepEqual(selectTags('10.0.0', ['9.0.0']), ['10.0.0', '10.0', '10', 'latest']);
  assert.deepEqual(selectTags('1.0.9007199254740992', ['1.0.9007199254740993']), ['1.0.9007199254740992']);
});

const image = 'ghcr.io/chrisromp/upfile';
const repository = 'chrisromp/upfile';
const tokenURL = 'https://ghcr.io/token?service=ghcr.io&scope=repository%3Achrisromp%2Fupfile%3Apull';
const listURL = 'https://ghcr.io/v2/chrisromp/upfile/tags/list?n=100';
const listResponse = (tags, link) => Response.json({ name: repository, tags }, {
  headers: link === undefined ? {} : { link },
});

function registryMock(responses) {
  let index = 0;
  return async (url, options) => {
    assert.ok(options.signal instanceof AbortSignal);
    assert.equal(options.redirect, 'error');
    assert.equal(options.headers.Accept, 'application/json');
    if (index++ === 0) {
      assert.equal(url.href, tokenURL);
      assert.equal(options.headers.Authorization, `Basic ${Buffer.from('actor:secret').toString('base64')}`);
      return Response.json({ token: 'registry-token' });
    }
    assert.equal(options.headers.Authorization, 'Bearer registry-token');
    const response = responses.shift();
    assert.ok(response, `Unexpected request to ${url.href}`);
    return typeof response === 'function' ? response(url) : response;
  };
}

test('reads all registry pages and uses their versions for alias selection', async () => {
  const responses = [
    url => {
      assert.equal(url.href, listURL);
      return listResponse(['main', '1.5.0'], `</v2/${repository}/tags/list?n=100&last=1.5.0>; rel="next"`);
    },
    url => {
      assert.equal(url.searchParams.get('last'), '1.5.0');
      return listResponse(['1.5.1', '2.0.0'], '<?n=100&last=2.0.0>; rel=next');
    },
    url => {
      assert.equal(url.searchParams.get('last'), '2.0.0');
      return listResponse(['latest']);
    },
  ];
  const tags = await fetchPublishedTags(image, 'actor', 'secret', registryMock(responses));
  assert.deepEqual(tags, ['main', '1.5.0', '1.5.1', '2.0.0', 'latest']);
  assert.deepEqual(selectTags('1.5.2', tags), ['1.5.2', '1.5', '1']);
  assert.equal(responses.length, 0);
});

test('accepts an empty repository and explicitly identified first publication', async () => {
  for (const response of [
    listResponse([]),
    listResponse(null),
    Response.json({ errors: [{ code: 'NAME_UNKNOWN' }] }, { status: 404 }),
  ]) {
    assert.deepEqual(await fetchPublishedTags(image, 'actor', 'secret', registryMock([response])), []);
  }
});

test('never treats authorization, registry, or network errors as empty release history', async () => {
  for (const status of [401, 403, 429, 500, 503]) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
      new Response('', { status }),
    ])), new RegExp(`HTTP ${status}`));
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', async () => new Response('', {
      status,
    })), new RegExp(`GHCR token request: HTTP ${status}`));
  }
  for (const errors of [
    [], [{ code: 'DENIED' }], [{ code: 'NAME_UNKNOWN' }, { code: 'DENIED' }],
  ]) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
      Response.json({ errors }, { status: 404 }),
    ])), /HTTP 404/);
  }
  await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', async () => {
    throw new Error('offline');
  }), /offline/);
  await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
    () => { throw new Error('connection lost'); },
  ])), /connection lost/);
});

test('validates registry responses and credentials', async () => {
  for (const body of [null, {}, { token: '' }, { token: 42 }]) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', async () => Response.json(body)),
      /no bearer token/);
  }
  for (const body of [
    null, {}, { name: repository }, { name: 'another/image', tags: [] },
    { name: repository, tags: '1.0.0' }, { name: repository, tags: [123] },
  ]) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
      Response.json(body),
    ])), /Malformed GHCR tag listing/);
  }
  const noRequest = async () => assert.fail('must not contact the registry');
  for (const invalid of [
    'docker.io/chrisromp/upfile', 'ghcr.io/ChrisRomp/upfile', `${image}:main`,
    'ghcr.io/chrisromp/../upfile', `${image}\n`,
  ]) {
    await assert.rejects(fetchPublishedTags(invalid, 'actor', 'secret', noRequest), /GHCR image name/);
  }
  await assert.rejects(fetchPublishedTags(image, '', 'secret', noRequest), /username and token/);
  await assert.rejects(fetchPublishedTags(image, 'actor', '', noRequest), /username and token/);
  let requests = 0;
  assert.deepEqual(await fetchPublishedTags(image, 'actor', 'secret', async () => {
    return requests++ === 0 ? Response.json({ access_token: 'token' }) : listResponse([]);
  }), []);
});

test('fails closed on unsafe, malformed, looping, or incomplete pagination', async () => {
  for (const link of [
    '<https://example.invalid/tags>; rel="next"',
    `<http://ghcr.io/v2/${repository}/tags/list>; rel="next"`,
    '<https://ghcr.io/v2/other/image/tags/list>; rel="next"',
    `<https://actor:secret@ghcr.io/v2/${repository}/tags/list>; rel="next"`,
    `</v2/${repository}/tags/list#fragment>; rel="next"`,
  ]) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
      listResponse(['1.0.0'], link),
    ])), /Unsafe GHCR pagination URL/);
  }
  for (const link of ['', 'not a link', '<next>; rel="last"']) {
    await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
      listResponse(['1.0.0'], link),
    ])), /Malformed GHCR pagination link/);
  }
  await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
    listResponse(['1.0.0'], `<${listURL}>; rel="next"`),
  ])), /repeated a page/);
  await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', registryMock([
    listResponse(['1.0.0'], '<?n=100&last=1.0.0>; rel="next"'),
    Response.json({ errors: [{ code: 'NAME_UNKNOWN' }] }, { status: 404 }),
  ])), /HTTP 404/);
});

test('bounds the number of registry pages', async () => {
  let pages = 0;
  const request = async url => {
    if (url.pathname === '/token') return Response.json({ token: 'token' });
    pages++;
    return listResponse([], `<?n=100&last=${pages}>; rel="next"`);
  };
  await assert.rejects(fetchPublishedTags(image, 'actor', 'secret', request), /exceeded 1000 pages/);
  assert.equal(pages, 1000);
});

test('CLI produces GitHub outputs, keeps main independent of GHCR, and reports failures', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'upfile-container-tags-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const script = new URL('./container-tags.mjs', import.meta.url);
  const output = join(directory, 'output');
  const run = (command, env, imports = []) => spawnSync(process.execPath, [
    ...imports, fileURLToPath(script), command,
  ], {
    encoding: 'utf8',
    env: { GITHUB_OUTPUT: output, ...env },
  });
  let result = run('channel', { GITHUB_EVENT_NAME: 'push', GITHUB_REF: 'refs/tags/v1.0.0' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(await readFile(output, 'utf8'), 'publish=true\nversion=1.0.0\n');
  await rm(output);
  result = run('channel', { GITHUB_EVENT_NAME: 'workflow_dispatch', GITHUB_REF: 'refs/tags/1.0.0' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(await readFile(output, 'utf8'), 'publish=false\nversion=\n');
  assert.match(result.stdout, /Only non-deleted push events/);
  await rm(output);
  result = run('tags', { IMAGE_VERSION: 'main' });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(await readFile(output, 'utf8'), 'tags<<CONTAINER_TAGS\nmain\nCONTAINER_TAGS\n');
  await rm(output);
  for (const [version, published, expected] of [
    ['1.0.0', [], ['1.0.0', '1.0', '1', 'latest']],
    ['1.5.1', ['1.5.0', '2.0.0'], ['1.5.1', '1.5', '1']],
    ['1.0.0', ['1.0.1'], ['1.0.0']],
  ]) {
    const mock = `
      import assert from 'node:assert/strict';
      globalThis.fetch = async url => {
        if (url.href === ${JSON.stringify(tokenURL)}) return Response.json({ token: 'test-token' });
        assert.equal(url.href, ${JSON.stringify(listURL)});
        return Response.json({ name: ${JSON.stringify(repository)}, tags: ${JSON.stringify(published)} });
      };
    `;
    result = run('tags', {
      IMAGE_VERSION: version, IMAGE_NAME: image, GITHUB_ACTOR: 'actor', GHCR_TOKEN: 'test-secret',
    }, ['--import', `data:text/javascript,${encodeURIComponent(mock)}`]);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(await readFile(output, 'utf8'),
      `tags<<CONTAINER_TAGS\n${expected.join('\n')}\nCONTAINER_TAGS\n`);
    assert.doesNotMatch(result.stdout + result.stderr, /test-secret|test-token/);
    await rm(output);
  }
  result = run('tags', { IMAGE_VERSION: '1.0.0' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /IMAGE_NAME is required/);
  await assert.rejects(readFile(output), { code: 'ENOENT' });
  result = run('tags', { IMAGE_VERSION: '1.0.0-rc.1' });
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Invalid stable image version/);
  result = run('unknown', {});
  assert.equal(result.status, 1);
  assert.match(result.stderr, /Usage:/);
});
