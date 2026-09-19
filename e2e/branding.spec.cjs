const { test, expect } = require('@playwright/test');

const id = 'c'.repeat(32);
const origin = surface => surface === 'admin'
  ? process.env.UPFILE_E2E_ADMIN_URL || 'https://localhost:8443/admin'
  : process.env.UPFILE_E2E_PUBLIC_URL || 'https://localhost:8443';
const settings = {
  configured: true, max_file_bytes: 1_000_000, storage_budget_bytes: 10_000_000,
  default_link_hours: 168, stored_bytes: 0, reserved_bytes: 0, chunk_bytes: 16_777_216,
  lease_seconds: 300, max_records: 100000, record_count: 0, cleanup_errors: 0,
};

async function mockSurface(page, surface, linkStatus = 200) {
  await page.route('**/api/**', async route => {
    const pathname = new URL(route.request().url()).pathname.replace(/^\/admin/, '');
    if (pathname === '/api/surface') {
      await route.fulfill({ json: { surface } });
    } else if (pathname === '/api/settings') {
      await route.fulfill({ json: settings });
    } else if (pathname === '/api/containers') {
      await route.fulfill({ json: { items: [], total: 0 } });
    } else if (pathname === `/api/links/${id}`) {
      await route.fulfill({ status: linkStatus, json: linkStatus === 200 ? {
        id, title: 'Public request', instructions: '', max_file_bytes: 1_000_000,
        chunk_bytes: 16_777_216, expires_at: Math.floor(Date.now() / 1000) + 3600,
        session_expires_at: Math.floor(Date.now() / 1000) + 3600,
        busy: false, reset_available: false,
      } : { code: 'session_required', error: 'Reopen the original shared link.' } });
    } else {
      throw new Error(`Unexpected branding request: ${pathname}`);
    }
  });
}

async function expectStaticBrand(page) {
  await expect(page.locator('.brand')).toBeVisible();
  await expect(page.getByRole('link', { name: 'upfile', exact: true })).toHaveCount(0);
  await expect(page.locator('.brand')).not.toHaveAttribute('href');
}

test('public branding does not navigate away or discard selected files', async ({ page }) => {
  await mockSurface(page, 'public');
  await page.goto(`${origin('public')}/u/${id}`);
  await expect(page.getByRole('heading', { name: 'Public request' })).toBeVisible();
  await expectStaticBrand(page);
  await page.locator('input[type=file]').setInputFiles({ name: 'keep.txt', mimeType: 'text/plain', buffer: Buffer.from('keep') });
  const url = page.url();
  await page.locator('.brand').click();
  expect(page.url()).toBe(url);
  await expect(page.getByRole('listitem').filter({ hasText: 'keep.txt' })).toBeVisible();
});

test('public missing-session and not-found surfaces have non-link branding', async ({ page }) => {
  await mockSurface(page, 'public', 401);
  await page.goto(`${origin('public')}/u/${id}`);
  await expect(page.getByRole('alert')).toBeVisible();
  await expectStaticBrand(page);
  await page.goto(`${origin('public')}/u/not-an-upload`);
  await expect(page.getByRole('heading', { name: 'Page not found' })).toBeVisible();
  await expectStaticBrand(page);
});

test('unknown loading and error surfaces do not assume an admin home page', async ({ page }) => {
  let release;
  const held = new Promise(resolve => { release = resolve; });
  await page.route('**/api/surface', async route => {
    await held;
    await route.fulfill({ status: 503, json: { code: 'unavailable', error: 'Temporarily unavailable.' } });
  });
  await page.goto(`${origin('public')}/u/${id}`, { waitUntil: 'domcontentloaded' });
  await expect(page.getByText('Opening upfile…', { exact: true })).toBeVisible();
  await expectStaticBrand(page);
  release();
  await expect(page.getByRole('alert')).toContainText('Temporarily unavailable.');
  await expectStaticBrand(page);
});

test('admin branding retains home navigation, including its wrong-surface page', async ({ page }) => {
  await mockSurface(page, 'admin');
  await page.goto(`${origin('admin')}/settings`);
  await expect(page.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();
  const home = page.getByRole('link', { name: 'upfile', exact: true });
  await expect(home).toHaveAttribute('href', '/admin/');
  await home.click();
  await expect(page.getByRole('heading', { name: 'File requests', exact: true })).toBeVisible();
  expect(new URL(page.url()).pathname).toBe('/admin/');
  await page.goto(`${origin('admin')}/u/${id}`);
  await expect(page.getByRole('heading', { name: 'Page not found' })).toBeVisible();
  await expect(home).toHaveAttribute('href', '/admin/');
});
