const { test, expect } = require('@playwright/test');

test('request tabs support roving focus, manual activation, and keyboard exit', async ({ page }) => {
  const origin = process.env.UPFILE_E2E_ADMIN_URL || 'https://localhost:8443/admin';
  const id = 'a'.repeat(32);
  let fileReads = 0;
  let linkReads = 0;
  await page.route('**/api/**', async route => {
    const path = new URL(route.request().url()).pathname.replace(/^\/admin/, '');
    if (path === '/api/surface') {
      await route.fulfill({ json: { surface: 'admin' } });
    } else if (path === '/api/settings') {
      await route.fulfill({ json: {
        configured: true, max_file_bytes: 2_000_000, storage_budget_bytes: 10_000_000,
        default_link_hours: 168, stored_bytes: 0, reserved_bytes: 0, chunk_bytes: 16_777_216,
        lease_seconds: 300, max_records: 100000, record_count: 0, cleanup_errors: 0,
      } });
    } else if (path === `/api/containers/${id}`) {
      await route.fulfill({ json: {
        id, name: 'Keyboard request', instructions: '', max_file_bytes: null,
        effective_max_bytes: 2_000_000, created_at: 1_700_000_000, status: 'active',
        file_count: 0, stored_bytes: 0, active_uploads: 0, active_downloads: 0, link_count: 0,
      } });
    } else if (path === `/api/containers/${id}/files`) {
      fileReads++;
      await route.fulfill({ json: { items: [], total: 0 } });
    } else if (path === `/api/containers/${id}/links`) {
      linkReads++;
      await route.fulfill({ json: { items: [], total: 0 } });
    } else {
      throw new Error(`Unexpected tab request: ${path}`);
    }
  });
  await page.goto(`${origin}/containers/${id}`);
  const files = page.getByRole('tab', { name: 'Received files' });
  const links = page.getByRole('tab', { name: 'Upload links' });
  const oneTabStop = () => expect(page.locator('[role=tab][tabindex="0"]')).toHaveCount(1);
  await expect(files).toHaveAttribute('aria-selected', 'true');
  await expect(files).toHaveAttribute('tabindex', '0');
  await expect(links).toHaveAttribute('tabindex', '-1');
  await expect(page.locator('#links-panel')).toBeAttached();
  await expect(page.locator('#links-panel')).toBeHidden();
  await page.getByRole('button', { name: 'Delete request', exact: true }).focus();
  await page.keyboard.press('Tab');
  await expect(files).toBeFocused();
  await files.press('ArrowRight');
  await expect(links).toBeFocused();
  await oneTabStop();
  await expect(files).toHaveAttribute('aria-selected', 'true');
  expect(linkReads).toBe(0);
  await links.press('ArrowRight');
  await expect(files).toBeFocused();
  await files.press('ArrowLeft');
  await expect(links).toBeFocused();
  await links.press('Home');
  await expect(files).toBeFocused();
  await files.press('End');
  await expect(links).toBeFocused();
  await links.press('Enter');
  await expect(links).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tabpanel', { name: 'Upload links' })).toBeVisible();
  await expect(page.locator('#files-panel')).toBeHidden();
  await expect.poll(() => linkReads).toBe(1);
  await links.press('Tab');
  await expect(page.getByRole('searchbox', { name: 'Search upload links' })).toBeFocused();
  await page.keyboard.press('Shift+Tab');
  await expect(links).toBeFocused();

  // Arrowing to an inactive tab does not replace the section or its data.
  const previousFileReads = fileReads;
  await links.press('ArrowLeft');
  await expect(files).toBeFocused();
  await expect(links).toHaveAttribute('aria-selected', 'true');
  expect(fileReads).toBe(previousFileReads);
  await files.press('Tab');
  await expect(page.getByRole('searchbox', { name: 'Search upload links' })).toBeFocused();
  await expect(links).toHaveAttribute('tabindex', '0');
  await page.keyboard.press('Shift+Tab');
  await expect(links).toBeFocused();
  await links.press('Home');
  await files.press('Space');
  await expect(files).toHaveAttribute('aria-selected', 'true');
  await expect(page.getByRole('tabpanel', { name: 'Received files' })).toBeVisible();
  await oneTabStop();
});
