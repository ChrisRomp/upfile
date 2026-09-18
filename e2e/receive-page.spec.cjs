const { test, expect } = require('@playwright/test');

const id = 'c'.repeat(32);
function linkInfo(busy = false) {
  return {
    id, title: 'September invoices', instructions: 'Please send your invoices.',
    max_file_bytes: 1_000_000, chunk_bytes: 16_777_216,
    expires_at: Math.floor(Date.now() / 1000) + 3600,
    session_expires_at: Math.floor(Date.now() / 1000) + 3600,
    busy, reset_available: busy,
  };
}

test('receive page labels the request and validates automatically before sending', async ({ page }) => {
  let checks = 0;
  let admission = false;
  await page.route('**/api/**', async route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/surface') {
      await route.fulfill({ json: { surface: 'public' } });
    } else if (pathname === `/api/links/${id}`) {
      checks++;
      if (checks === 1) {
        await route.fulfill({ json: linkInfo() });
      } else {
        await route.fulfill({ status: 410, json: { code: 'link_unavailable', error: 'The link has been revoked.' } });
      }
    } else {
      admission = true;
      throw new Error(`Should not send bytes after the link was revoked: ${pathname}`);
    }
  });
  await page.goto(`https://localhost:8443/u/${id}`);
  await expect(page.getByText('File request', { exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'September invoices' })).toBeVisible();
  await expect(page.getByText(/sending files to/i)).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Check link status', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(0);
  await page.locator('input[type=file]').setInputFiles({ name: 'invoice.txt', mimeType: 'text/plain', buffer: Buffer.from('invoice') });
  await page.getByRole('button', { name: 'Send 1 file', exact: true }).click();
  await expect(page.getByRole('alert').first()).toContainText(/expired|revoked/);
  expect(checks).toBe(2);
  expect(admission).toBe(false);
});

test('interrupted uploads retain recovery checks without a normal status button', async ({ page }) => {
  let busy = true;
  await page.route('**/api/**', async route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/surface') {
      await route.fulfill({ json: { surface: 'public' } });
    } else if (pathname === `/api/links/${id}`) {
      await route.fulfill({ json: linkInfo(busy) });
    } else {
      throw new Error(`Unexpected recovery request: ${pathname}`);
    }
  });
  await page.goto(`https://localhost:8443/u/${id}`);
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Reset unfinished upload', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Check link status', exact: true })).toHaveCount(0);
  busy = false;
  await page.getByRole('button', { name: 'Check status', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(0);
  await expect(page.locator('input[type=file]')).toBeEnabled();
});
