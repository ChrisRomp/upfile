const { test, expect } = require('@playwright/test');
const { randomUUID } = require('node:crypto');
const fs = require('node:fs/promises');

const admin = 'https://localhost:8443/admin';
const publicOrigin = 'https://localhost:8443';
const mutation = { Origin: publicOrigin, 'X-Upfile-Request': '1' };

async function call(request, method, path, data) {
  const response = await request.fetch(admin + path, { method, headers: mutation, data });
  expect(response.ok(), `${method} ${path}: ${await response.text()}`).toBeTruthy();
  return response.json();
}

async function fixture(request) {
  await call(request, 'PUT', '/api/settings', {
    max_file_bytes: 64 * 1024 * 1024, storage_budget_bytes: 512 * 1024 * 1024, default_link_hours: 168,
  });
  const container = await call(request, 'POST', '/api/containers', {
    name: `Browser check ${randomUUID().slice(0, 8)}`,
    instructions: 'Send your files here.', max_file_bytes: null,
  });
  const link = await call(request, 'POST', `/api/containers/${container.id}/links`, {
    sender_label: 'Expected sender', expires_at: Math.floor(Date.now() / 1000) + 3600, max_file_bytes: null,
  });
  return { container, link };
}

test('multi-file retry, link reuse, progress, safe downloads, rename and delete', async ({ page, context, request }) => {
  const { container, link } = await fixture(request);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  let patches = 0;
  const patchIDs = [];
  let admissions = 0;
  page.on('request', r => {
    if (r.method() === 'POST' && /\/attempts$/.test(r.url())) admissions++;
  });
  await page.route('**/uploads/*', async route => {
    if (route.request().method() === 'PATCH') {
      patches++;
      patchIDs.push(route.request().url());
      if (patches === 1) {
        await route.fulfill({ status: 503, contentType: 'application/json', body: '{"code":"internal","error":"Temporary test interruption"}' });
        return;
      }
    }
    await route.continue();
  });
  await page.goto(link.url);
  await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
  expect(page.url()).not.toContain('#');
  await page.locator('input[type=file]').setInputFiles([
    { name: 'alpha.txt', mimeType: 'text/plain', buffer: Buffer.from('first file') },
    { name: 'beta.txt', mimeType: 'text/plain', buffer: Buffer.from('second file') },
  ]);
  await page.getByRole('textbox', { name: /^Comment/ }).first().fill('<script>alert("plain text")</script>');
  await page.getByRole('button', { name: 'Send 2 files' }).click();
  await expect(page.getByText('2 received · 0 failed · 0 canceled · 0 waiting')).toBeVisible({ timeout: 30000 });
  await expect(page.getByRole('progressbar', { name: 'Overall byte progress' })).toHaveJSProperty('position', 1);
  expect(patches).toBeGreaterThanOrEqual(3);
  expect(patchIDs[0]).toBe(patchIDs[1]);
  expect(patchIDs.at(-1)).not.toBe(patchIDs[0]);
  expect(admissions).toBe(2);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBeTruthy();
  expect(await page.evaluate(() => Object.keys(localStorage))).toEqual([]);
  const cookies = (await context.cookies(publicOrigin)).filter(c => c.name.startsWith('__Host-upfile_'));
  expect(cookies.some(c => c.httpOnly && c.secure && c.sameSite === 'Strict')).toBeTruthy();

  // Refresh clears the selected queue but keeps the link-specific session valid.
  await page.reload();
  await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
  await page.locator('input[type=file]').setInputFiles({ name: 'again.txt', mimeType: 'text/plain', buffer: Buffer.from('again') });
  await page.getByRole('button', { name: 'Send 1 file', exact: true }).click();
  await expect(page.getByText('1 received · 0 failed · 0 canceled · 0 waiting')).toBeVisible();

  const files = await call(request, 'GET', `/api/containers/${container.id}/files`);
  expect(files.total).toBe(3);
  const alpha = files.items.find(f => f.name === 'alpha.txt');
  expect(alpha.comment).toBe('<script>alert("plain text")</script>');
  expect(alpha.sender_label).toBe('Expected sender');
  const bytes = await request.get(`${admin}/api/files/${alpha.id}/download`);
  expect(await bytes.text()).toBe('first file');
  expect(bytes.headers()['content-disposition']).toContain('attachment');
  expect(bytes.headers()['content-type']).toBe('application/octet-stream');
  const partial = await request.get(`${admin}/api/files/${alpha.id}/download`, { headers: { Range: 'bytes=0-4' } });
  expect(partial.status()).toBe(206);
  expect(await partial.text()).toBe('first');

  const adminPage = await context.newPage();
  await adminPage.goto(`${admin}/containers/${container.id}`);
  const row = adminPage.getByRole('row').filter({ hasText: 'alpha.txt' });
  await expect(row).toBeVisible();
  await row.getByRole('button', { name: 'Rename', exact: true }).click();
  const dialog = adminPage.getByRole('dialog');
  await dialog.getByRole('textbox').fill('renamed.txt');
  await dialog.getByRole('button', { name: /Save|Rename/ }).click();
  await expect(adminPage.getByRole('row').filter({ hasText: 'renamed.txt' })).toBeVisible();
  const downloadEvent = adminPage.waitForEvent('download');
  await adminPage.getByRole('row').filter({ hasText: 'renamed.txt' }).getByRole('button', { name: 'Download' }).click();
  await adminPage.getByRole('dialog').getByRole('link', { name: 'Download file' }).click();
  const download = await downloadEvent;
  expect(download.suggestedFilename()).toBe('renamed.txt');
  expect(await fs.readFile(await download.path(), 'utf8')).toBe('first file');
  await call(request, 'DELETE', `/api/containers/${container.id}`);
  await page.reload();
  await expect(page.getByRole('alert')).toContainText(/expired|revoked|unavailable/);
  expect(errors).toEqual([]);
});

test('multiple links in separate tabs, missing secret, and mid-queue revocation', async ({ page, context, request, browser }) => {
  const { container, link } = await fixture(request);
  const second = await call(request, 'POST', `/api/containers/${container.id}/links`, {
    sender_label: 'Second sender', expires_at: Math.floor(Date.now() / 1000) + 3600, max_file_bytes: null,
  });
  await page.goto(link.url);
  await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
  const other = await context.newPage();
  await other.goto(second.url);
  await expect(other.getByRole('heading', { name: container.name })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
  const anonymous = await browser.newContext({ ignoreHTTPSErrors: true });
  const noSecret = await anonymous.newPage();
  await noSecret.goto(link.url.split('#')[0]);
  await expect(noSecret.getByRole('alert')).toContainText(/original.*link/i);
  await anonymous.close();
  let revoked = false;
  await page.route('**/uploads/*', async route => {
    if (route.request().method() === 'PATCH' && !revoked) {
      revoked = true;
      await call(request, 'POST', `/api/links/${link.id}/revoke`, {});
    }
    await route.continue();
  });
  await page.locator('input[type=file]').setInputFiles([
    { name: 'stopped.txt', mimeType: 'text/plain', buffer: Buffer.from('must not complete') },
    { name: 'queued.txt', mimeType: 'text/plain', buffer: Buffer.from('must not start') },
  ]);
  await page.getByRole('button', { name: 'Send 2 files' }).click();
  await expect(page.getByRole('alert').first()).toBeVisible();
  const files = await call(request, 'GET', `/api/containers/${container.id}/files`);
  expect(files.total).toBe(0);
  await call(request, 'DELETE', `/api/containers/${container.id}`);
});

test('cancel an active file without undoing received files', async ({ page, request }) => {
  const { container, link } = await fixture(request);
  await page.goto(link.url);
  await expect(page.getByRole('heading', { name: container.name })).toBeVisible();
  await page.locator('input[type=file]').setInputFiles({name:'kept.txt',mimeType:'text/plain',buffer:Buffer.from('keep')});
  await page.getByRole('button', {name:'Send 1 file',exact:true}).click();
  await expect(page.getByText('1 received · 0 failed · 0 canceled · 0 waiting')).toBeVisible();
  let started;
  const firstPatch = new Promise(resolve => {started=resolve;});
  let release;
  const held = new Promise(resolve => {release=resolve;});
  await page.route('**/uploads/*', async route => {
    if(route.request().method()==='PATCH') {
      started();
      await held;
      await route.abort();
      return;
    }
    await route.continue();
  });
  await page.locator('input[type=file]').setInputFiles({name:'canceled.txt',mimeType:'text/plain',buffer:Buffer.alloc(1024,7)});
  await page.getByRole('button', {name:'Send 1 file',exact:true}).click();
  await firstPatch;
  await page.getByRole('listitem').filter({hasText:'canceled.txt'}).getByRole('button',{name:/Cancel/}).click();
  release();
  await expect(page.getByText('1 received · 0 failed · 1 canceled · 0 waiting')).toBeVisible();
  const files=await call(request,'GET',`/api/containers/${container.id}/files`);
  expect(files.total).toBe(1);
  expect(files.items[0].name).toBe('kept.txt');
  await call(request,'DELETE',`/api/containers/${container.id}`);
});
