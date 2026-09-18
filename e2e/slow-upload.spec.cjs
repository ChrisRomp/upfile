const { test, expect } = require('@playwright/test');
const { createHash, randomUUID } = require('node:crypto');

test('a progressing PATCH can exceed 60 seconds without retrying', async ({ page, context, request }, testInfo) => {
  test.skip(process.env.UPFILE_SLOW_UPLOAD_TEST !== '1' || testInfo.project.name !== 'desktop',
    'Opt-in real-time slow-uplink check against the isolated development fixture.');
  test.setTimeout(150000);
  const admin = 'https://localhost:8444';
  const headers = { Origin: admin, 'X-Upfile-Request': '1' };
  async function api(method, path, data) {
    const response = await request.fetch(admin + path, { method, headers, data });
    expect(response.ok(), `${method} ${path}: ${await response.text()}`).toBeTruthy();
    return response.json();
  }
  const previous = await api('GET', '/api/settings');
  let container;
  let network;
  const payload = Buffer.alloc(640 * 1024, 97);
  const patches = [];
  try {
    await api('PUT', '/api/settings', {
      max_file_bytes: Math.max(previous.max_file_bytes, payload.length),
      storage_budget_bytes: Math.max(previous.storage_budget_bytes, previous.stored_bytes + previous.reserved_bytes + 2 * payload.length),
      default_link_hours: previous.default_link_hours,
    });
    container = await api('POST', '/api/containers', {
      name: `Slow upload ${randomUUID()}`, instructions: '', max_file_bytes: null,
    });
    page.on('request', r => {
      if (r.method() === 'PATCH') patches.push({ url: r.url(), started: Date.now() });
    });
    await page.goto(container.initial_link.url);
    await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
    await page.locator('input[type=file]').setInputFiles({
      name: 'slow.txt', mimeType: 'text/plain', buffer: payload,
    });
    network = await context.newCDPSession(page);
    await network.send('Network.enable');
    await network.send('Network.emulateNetworkConditions', {
      offline: false, latency: 0, downloadThroughput: -1, uploadThroughput: 8 * 1024,
    });
    await page.getByRole('button', { name: 'Send 1 file', exact: true }).click();
    const progress = page.getByRole('progressbar', { name: 'Upload progress for slow.txt', exact: true });
    await expect(progress).toBeVisible();
    await expect.poll(async () => progress.evaluate(element => element.value), { timeout: 15000 }).toBeGreaterThan(0);
    await expect(page.getByText('1 received · 0 failed · 0 canceled · 0 waiting')).toBeVisible({ timeout: 125000 });
    expect(patches).toHaveLength(1);
    const elapsed = Date.now() - patches[0].started;
    expect(elapsed).toBeGreaterThan(60000);
    const files = await api('GET', `/api/containers/${container.id}/files`);
    expect(files.total).toBe(1);
    const download = await request.get(`${admin}/api/files/${files.items[0].id}/download`);
    expect(download.ok()).toBeTruthy();
    const digest = bytes => createHash('sha256').update(bytes).digest('hex');
    expect(digest(await download.body())).toBe(digest(payload));
    console.log(`Progressing PATCH completed in ${(elapsed / 1000).toFixed(1)}s without retry; SHA-256 verified.`);
  } finally {
    if (network) await network.detach();
    if (container) await api('DELETE', `/api/containers/${container.id}`);
    if (previous.configured) await api('PUT', '/api/settings', {
      max_file_bytes: previous.max_file_bytes,
      storage_budget_bytes: previous.storage_budget_bytes,
      default_link_hours: previous.default_link_hours,
    });
  }
});
