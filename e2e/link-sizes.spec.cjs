const { test, expect } = require('@playwright/test');

for (const editing of [false, true]) {
  test(`${editing ? 'edit' : 'create'} upload link uses MB and inherits the request limit`, async ({ page }) => {
    const containerID = 'a'.repeat(32);
    const linkID = 'b'.repeat(32);
    const settings = {
      configured: true, max_file_bytes: 4_000_000, storage_budget_bytes: 10_000_000,
      default_link_hours: 168, stored_bytes: 0, reserved_bytes: 0, chunk_bytes: 16_777_216,
      lease_seconds: 300, max_records: 100000, record_count: 0, cleanup_errors: 0,
    };
    const container = {
      id: containerID, name: 'MB request', instructions: '', max_file_bytes: 2_000_000,
      effective_max_bytes: 2_000_000, created_at: 1_700_000_000, status: 'active',
      file_count: 0, stored_bytes: 0, active_uploads: 0, active_downloads: 0, link_count: 1,
    };
    const link = {
      id: linkID, container_id: containerID, sender_label: 'MB sender',
      max_file_bytes: 1_048_577, effective_max_bytes: 1_048_577,
      expires_at: Math.floor(Date.now() / 1000) + 3600, created_at: 1_700_000_000,
      status: 'active', file_count: 0, active_uploads: 0,
    };
    const saved = [];
    await page.route('**/api/**', async route => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname;
      if (pathname === '/api/surface') {
        await route.fulfill({ json: { surface: 'admin' } });
      } else if (pathname === '/api/settings') {
        await route.fulfill({ json: settings });
      } else if (pathname === `/api/containers/${containerID}`) {
        await route.fulfill({ json: container });
      } else if (pathname === `/api/containers/${containerID}/files`) {
        await route.fulfill({ json: { items: [], total: 0 } });
      } else if (pathname === `/api/containers/${containerID}/links` || pathname === `/api/links/${linkID}`) {
        if (request.method() === 'POST' || request.method() === 'PUT') {
          const body = request.postDataJSON();
          saved.push(body);
          Object.assign(link, body, { effective_max_bytes: body.max_file_bytes ?? container.effective_max_bytes });
          await route.fulfill({ json: {
            ...link,
            ...(request.method() === 'POST' ? { url: `https://localhost:8443/u/${linkID}#fixture-only` } : {}),
          } });
        } else {
          await route.fulfill({ json: { items: editing || saved.length ? [link] : [], total: editing || saved.length ? 1 : 0 } });
        }
      } else {
        throw new Error(`Unexpected link request: ${request.method()} ${pathname}`);
      }
    });

    await page.goto(`https://localhost:8444/containers/${containerID}`);
    await page.getByRole('tab', { name: 'Upload links' }).click();
    const open = () => page.getByRole('button', { name: editing ? 'Edit' : '＋ Create link', exact: true }).click();
    await open();
    const dialog = page.getByRole('dialog');
    const size = dialog.getByRole('spinbutton', { name: /^Maximum file size \(MB, optional\)/ });
    await expect(size).toHaveValue(editing ? '1.048577' : '');
    await expect(size).toHaveAttribute('placeholder', 'Inherit 2 MB');
    await expect(size).toHaveAttribute('max', '2');
    await expect(dialog.getByText(/Leave blank to inherit 2 MB/)).toBeVisible();
    await expect(dialog.getByRole('spinbutton', { name: /\(bytes/ })).toHaveCount(0);
    await dialog.getByRole('textbox', { name: /^Sender label/ }).fill('MB sender');
    const submit = () => dialog.getByRole('button', { name: editing ? 'Save link' : 'Create link', exact: true }).click();
    await submit();
    await expect.poll(() => saved.length).toBe(1);
    expect(saved[0].max_file_bytes).toBe(editing ? 1_048_577 : null);
    if (!editing) await dialog.getByRole('button', { name: 'Done', exact: true }).click();

    await open();
    await dialog.getByRole('textbox', { name: /^Sender label/ }).fill('MB sender');
    await size.fill('2.000001');
    await submit();
    expect(saved.length).toBe(1);
    expect(await size.evaluate(input => input.validity.rangeOverflow)).toBe(true);
    await size.fill(editing ? '' : '1.25');
    await submit();
    await expect.poll(() => saved.length).toBe(2);
    expect(saved[1].max_file_bytes).toBe(editing ? null : 1_250_000);
  });
}
