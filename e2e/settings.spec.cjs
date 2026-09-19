const { test, expect } = require('@playwright/test');

for (const configured of [false, true]) {
  test(`settings use MB with ${configured ? 'existing byte limits' : 'blank first-run inputs'}`, async ({ page }) => {
    const settings = {
      configured,
      max_file_bytes: configured ? 1_073_741_824 : 0,
      storage_budget_bytes: configured ? 10_737_418_240 : 0,
      default_link_hours: 168,
      stored_bytes: 0, reserved_bytes: 0, chunk_bytes: 16_777_216,
      lease_seconds: 300, max_records: 100000, record_count: 0, cleanup_errors: 0,
    };
    const saved = [];
    // Mock only APIs; load the actual built application without changing the user's settings.
    await page.route('**/api/**', async route => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname.replace(/^\/admin/, '');
      if (pathname === '/api/surface') {
        await route.fulfill({ json: { surface: 'admin' } });
      } else if (pathname === '/api/settings') {
        if (request.method() === 'PUT') {
          const body = request.postDataJSON();
          saved.push(body);
          Object.assign(settings, body, { configured: true });
        }

        await route.fulfill({ json: settings });
      } else {
        throw new Error(`Unexpected settings request: ${request.method()} ${pathname}`);
      }
    });

    await page.goto('https://localhost:8443/admin/settings');
    const maximum = page.getByRole('spinbutton', { name: /^Global maximum file size \(MB\)/ });
    const budget = page.getByRole('spinbutton', { name: /^Total storage budget \(MB\)/ });
    await expect(maximum).toHaveValue(configured ? '1073.741824' : '');
    await expect(budget).toHaveValue(configured ? '10737.41824' : '');
    await expect(page.getByText('1 MB = 1,000,000 bytes. Decimals are allowed.').first()).toBeVisible();

    if (configured) {
      await page.getByRole('button', { name: 'Save settings', exact: true }).click();
      await expect.poll(() => saved.length).toBe(1);
      expect(saved[0]).toEqual({
        max_file_bytes: 1_073_741_824, storage_budget_bytes: 10_737_418_240, default_link_hours: 168,
      });
    }
    await maximum.fill('1.5');
    await budget.fill('2000');
    await page.getByRole('button', { name: configured ? 'Save settings' : 'Save and get started', exact: true }).click();
    await expect.poll(() => saved.length).toBe(configured ? 2 : 1);
    expect(saved.at(-1)).toEqual({
      max_file_bytes: 1_500_000, storage_budget_bytes: 2_000_000_000, default_link_hours: 168,
    });
    await page.reload();
    await expect(maximum).toHaveValue('1.5');
    await expect(budget).toHaveValue('2000');
  });
}

for (const editing of [false, true]) {
  test(`${editing ? 'edit' : 'new'} request uses optional MB limits and preserves inheritance`, async ({ page }) => {
    const settings = {
      configured: true, max_file_bytes: 2_000_000, storage_budget_bytes: 10_000_000,
      default_link_hours: 168, stored_bytes: 0, reserved_bytes: 0, chunk_bytes: 16_777_216,
      lease_seconds: 300, max_records: 100000, record_count: 0, cleanup_errors: 0,
    };
    const container = {
      id: 'a'.repeat(32), name: 'MB request', instructions: '', max_file_bytes: 1_048_577,
      effective_max_bytes: 1_048_577, created_at: 1_700_000_000, status: 'active',
      file_count: 0, stored_bytes: 0, active_uploads: 0, active_downloads: 0, link_count: 0,
    };
    const saved = [];
    await page.route('**/api/**', async route => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname.replace(/^\/admin/, '');
      if (pathname === '/api/surface') {
        await route.fulfill({ json: { surface: 'admin' } });
      } else if (pathname === '/api/settings') {
        await route.fulfill({ json: settings });
      } else if (pathname === '/api/containers' || pathname === `/api/containers/${container.id}`) {
        if (request.method() === 'POST' || request.method() === 'PUT') {
          const body = request.postDataJSON();
          saved.push(body);
          Object.assign(container, body, { effective_max_bytes: body.max_file_bytes ?? settings.max_file_bytes });
          await route.fulfill({ json: request.method() === 'POST' ? {
            ...container,
            initial_link: { url: `https://localhost:8443/u/${'b'.repeat(32)}#new-request-secret` },
          } : container });
        } else {
          await route.fulfill({ json: pathname === '/api/containers' ? { items: [], total: 0 } : container });
        }
      } else if (pathname === `/api/containers/${container.id}/files`) {
        await route.fulfill({ json: { items: [], total: 0 } });
      } else {
        throw new Error(`Unexpected request: ${request.method()} ${pathname}`);
      }
    });
    await page.goto(`https://localhost:8443/admin${editing ? `/containers/${container.id}` : '/'}`);
    const open = () => page.getByRole('button', { name: editing ? 'Edit request' : /New request/ }).click();
    await open();
    const dialog = page.getByRole('dialog');
    const size = dialog.getByRole('spinbutton', { name: /^Maximum file size \(MB, optional\)/ });
    await expect(size).toHaveValue(editing ? '1.048577' : '');
    await expect(size).toHaveAttribute('placeholder', 'Inherit 2 MB');
    await expect(size).toHaveAttribute('max', '2');
    await expect(dialog.getByText(/Leave blank to inherit 2 MB/)).toBeVisible();
    await dialog.getByRole('textbox', { name: 'Request name', exact: true }).fill('MB request');
    await dialog.getByRole('button', { name: editing ? 'Save request' : 'Create request', exact: true }).click();
    await expect.poll(() => saved.length).toBe(1);
    expect(saved[0].max_file_bytes).toBe(editing ? 1_048_577 : null);
    if (!editing) {
      await expect(page.getByRole('heading', { name: 'Your upload link is ready' })).toBeVisible();
      await expect(page.getByRole('textbox', { name: 'Private upload link' })).toHaveValue(
        `https://localhost:8443/u/${'b'.repeat(32)}#new-request-secret`,
      );
      await page.getByRole('button', { name: 'Done', exact: true }).click();
    }

    await open();
    await dialog.getByRole('textbox', { name: 'Request name', exact: true }).fill('MB request');
    await size.fill(editing ? '' : '2.000001');
    if (!editing) {
      await dialog.getByRole('button', { name: 'Create request', exact: true }).click();
      expect(saved.length).toBe(1);
      expect(await size.evaluate(input => input.validity.rangeOverflow)).toBe(true);
      await size.fill('1.25');
    }
    await dialog.getByRole('button', { name: editing ? 'Save request' : 'Create request', exact: true }).click();
    await expect.poll(() => saved.length).toBe(2);
    expect(saved[1].max_file_bytes).toBe(editing ? null : 1_250_000);
  });
}
