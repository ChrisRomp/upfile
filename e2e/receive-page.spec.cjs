const { test, expect } = require('@playwright/test');

const id = 'c'.repeat(32);
const publicURL = `${process.env.UPFILE_E2E_PUBLIC_URL || 'https://localhost:8443'}/u/${id}`;
function linkInfo(busy = false, reset_available = busy) {
  return {
    id, title: 'September invoices', instructions: 'Please send your invoices.',
    max_file_bytes: 1_000_000, chunk_bytes: 16_777_216,
    expires_at: Math.floor(Date.now() / 1000) + 3600,
    session_expires_at: Math.floor(Date.now() / 1000) + 3600,
    busy, reset_available,
  };
}

async function mockLink(page, info, handleRequest) {
  await page.route('**/api/**', async route => {
    const pathname = new URL(route.request().url()).pathname;
    if (pathname === '/api/surface') {
      await route.fulfill({ json: { surface: 'public' } });
    } else if (pathname === `/api/links/${id}`) {
      await route.fulfill({ json: info() });
    } else if (handleRequest) {
      await handleRequest(route, pathname);
    } else {
      throw new Error(`Unexpected request: ${route.request().method()} ${pathname}`);
    }
  });
}

function file(name, contents = '') {
  return { name, mimeType: 'text/plain', buffer: Buffer.from(contents) };
}

async function expectProgress(page, value, max, percent) {
  const progress = page.getByRole('progressbar', { name: 'Overall byte progress', exact: true });
  await expect(progress).toHaveJSProperty('value', value);
  await expect(progress).toHaveJSProperty('max', max);
  await expect(page.locator('.queue-summary .section-heading')).toHaveText(`Overall progress${percent}%`);
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
  await page.goto(publicURL);
  await expect(page.getByText('File request', { exact: true })).toBeVisible();
  await expect(page.getByRole('heading', { name: 'September invoices' })).toBeVisible();
  await expect(page.getByText(/sending files to/i)).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Check link status', exact: true })).toHaveCount(0);
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(0);
  await page.locator('input[type=file]').setInputFiles({ name: 'invoice.txt', mimeType: 'text/plain', buffer: Buffer.from('invoice') });
  await expect(page.getByRole('alert').first()).toContainText(/expired|revoked/);
  await expect(page.getByRole('button', { name: /^Send / })).toHaveCount(0);
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
  await page.goto(publicURL);
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Reset unfinished upload', exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Check link status', exact: true })).toHaveCount(0);
  busy = false;
  await page.getByRole('button', { name: 'Check status', exact: true }).click();
  await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(0);
  await expect(page.locator('input[type=file]')).toBeEnabled();
});

for (const busy of [false, true]) {
  for (const reset of [false, true]) {
    test(`recovery controls for busy=${busy}, reset_available=${reset}`, async ({ page }) => {
      await mockLink(page, () => linkInfo(busy, reset));
      await page.goto(publicURL);
      await expect(page.getByRole('heading', { name: 'September invoices' })).toBeVisible();
      await expect(page.getByRole('button', { name: 'Check link status', exact: true })).toHaveCount(0);
      await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(busy || reset ? 1 : 0);
      await expect(page.getByRole('button', { name: 'Reset unfinished upload', exact: true })).toHaveCount(reset ? 1 : 0);
      await expect(page.locator('input[type=file]')).toBeEnabled({ enabled: !busy });
      await expect(page.getByRole('progressbar')).toHaveCount(0);
      if (busy) {
        await expect(page.getByText(reset ? 'An unfinished upload is blocking this link.' : 'This link has an active upload.', { exact: true })).toBeVisible();
      } else if (reset) {
        await expect(page.getByText('An abandoned upload is available to clean up.', { exact: true })).toBeVisible();
        await expect(page.getByText(/You can send new files or discard the abandoned partial upload/)).toBeVisible();
        await expect(page.getByText(/blocking this link/)).toHaveCount(0);
      }
    });
  }
}

for (const busy of [false, true]) {
  test(`resetting a ${busy ? 'blocking stale' : 'nonblocking abandoned'} upload requires confirmation and refreshes status`, async ({ page }) => {
    let info = linkInfo(busy, true);
    let checks = 0;
    let resets = 0;
    await mockLink(page, () => { checks++; return info; }, async (route, pathname) => {
      expect(pathname).toBe(`/api/links/${id}/reset`);
      expect(route.request().method()).toBe('POST');
      expect(route.request().postDataJSON()).toEqual({});
      expect(route.request().headers()['x-upfile-request']).toBe('1');
      resets++;
      info = linkInfo();
      await route.fulfill({ json: { ok: true } });
    });
    await page.goto(publicURL);
    await page.getByRole('button', { name: 'Reset unfinished upload', exact: true }).click();
    const dialog = page.getByRole('dialog', { name: 'Discard the unfinished upload?' });
    await expect(dialog).toContainText('This discards the stale partial file.');
    await expect(dialog).toContainText('remove completed deliveries');
    expect(resets).toBe(0);
    await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
    expect(resets).toBe(0);
    await page.getByRole('button', { name: 'Reset unfinished upload', exact: true }).click();
    await dialog.getByRole('button', { name: 'Reset upload', exact: true }).click();
    await expect(dialog).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Check status', exact: true })).toHaveCount(0);
    await expect(page.getByRole('button', { name: 'Reset unfinished upload', exact: true })).toHaveCount(0);
    await expect(page.locator('input[type=file]')).toBeEnabled();
    expect(resets).toBe(1);
    expect(checks).toBe(2);
  });
}

test('a reset rejected after server state changes keeps the confirmation and can refresh recovery status', async ({ page }) => {
  let info = linkInfo(false, true);
  let resets = 0;
  await mockLink(page, () => info, async (route, pathname) => {
    expect(pathname).toBe(`/api/links/${id}/reset`);
    expect(route.request().method()).toBe('POST');
    resets++;
    info = linkInfo(true, false);
    await route.fulfill({ status: 409, json: { code: 'busy', error: 'This upload is still active.' } });
  });
  await page.goto(publicURL);
  await page.getByRole('button', { name: 'Reset unfinished upload', exact: true }).click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('button', { name: 'Reset upload', exact: true }).click();
  await expect(dialog.getByRole('alert')).toHaveText('This upload is still active.');
  await expect(dialog).toBeVisible();
  expect(resets).toBe(1);
  await dialog.getByRole('button', { name: 'Cancel', exact: true }).click();
  await page.getByRole('button', { name: 'Check status', exact: true }).click();
  await expect(page.getByText('This link has an active upload.', { exact: true })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Reset unfinished upload', exact: true })).toHaveCount(0);
  await expect(page.locator('input[type=file]')).toBeDisabled();
});

test('selection starts uploads and dropped files join the queue while comments save', async ({ page }) => {
  const admissions = [];
  const comments = [];
  let release;
  await mockLink(page, () => linkInfo(), async (route, pathname) => {
    if (pathname.endsWith('/attempts')) {
      admissions.push(route.request().postDataJSON());
      const attempt = admissions.length === 1 ? 'd'.repeat(32) : 'e'.repeat(32);
      await route.fulfill({ json: {
        id: attempt, status: admissions.length === 1 ? 'uploading' : 'completed', size: 7, offset: 0,
        upload_url: `${new URL(publicURL).origin}/api/links/${id}/uploads/${attempt}`,
      } });
    } else if (pathname.endsWith('/comment')) {
      expect(route.request().method()).toBe('PUT');
      expect(route.request().headers()['x-upfile-request']).toBe('1');
      comments.push(route.request().postDataJSON().comment);
      await route.fulfill({ json: { comment: comments.at(-1) } });
    } else if (pathname.includes('/uploads/')) {
      if (route.request().method() === 'PATCH') await new Promise(resolve => { release = resolve; });
      await route.fulfill({
        status: route.request().method() === 'HEAD' ? 200 : 204,
        headers: { 'Tus-Resumable': '1.0.0', 'Upload-Length': '7', 'Upload-Offset': route.request().method() === 'HEAD' ? '0' : '7' },
      });
    } else {
      expect(pathname).toBe(`/api/links/${id}/attempts/${'d'.repeat(32)}`);
      await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size: 7, offset: 7 } });
    }
  });
  await page.goto(publicURL);
  await expect(page.getByText('File request', { exact: true })).toBeVisible();
  await expect(page.getByText('No files selected yet.', { exact: true })).toBeVisible();
  await page.locator('input[type=file]').setInputFiles(file('invoice.txt', 'invoice'));
  await expect.poll(() => typeof release).toBe('function');
  await expect(page.locator('input[type=file]')).toBeEnabled();
  const first = page.getByRole('listitem').filter({ hasText: 'invoice.txt' });
  await first.getByRole('textbox').fill('September');
  await expect(first.getByText('Comment saved.', { exact: true })).toBeVisible();
  expect(comments).toEqual(['September']);
  await page.locator('.drop-area').evaluate(element => {
    const dataTransfer = new DataTransfer();
    dataTransfer.items.add(new File(['receipt'], 'dropped.txt', { type: 'text/plain' }));
    element.dispatchEvent(new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer }));
  });
  const second = page.getByRole('listitem').filter({ hasText: 'dropped.txt' });
  await second.getByRole('textbox').fill('Queued note');
  expect(admissions).toHaveLength(1);
  await expect(second).toContainText('queued');
  release();
  await expect(page.locator('.summary-counts')).toHaveText('2 received · 0 failed · 0 canceled · 0 waiting');
  await expect(second.getByText('Comment saved.', { exact: true })).toBeVisible();
  expect(admissions.map(item => item.name)).toEqual(['invoice.txt', 'dropped.txt']);
  expect(comments).toEqual(['September', 'Queued note']);
  await first.getByRole('textbox').fill('Updated after receipt');
  await expect(first.getByText('Comment saved.', { exact: true })).toBeVisible();
  await first.getByRole('textbox').fill('');
  await expect(first.getByText('Comment saved.', { exact: true })).toBeVisible();
  expect(comments).toEqual(['September', 'Queued note', 'Updated after receipt', '']);
  await expect(page.getByRole('button', { name: /^Send / })).toHaveCount(0);
  await expect(page.getByRole('button', { name: /Check.*status|Reset unfinished upload/ })).toHaveCount(0);
  await expectProgress(page, 14, 14, 100);
  await page.getByRole('button', { name: 'Clear finished', exact: true }).click();
  await expect(page.getByText('No files selected yet.', { exact: true })).toBeVisible();
  await expect(page.getByRole('progressbar')).toHaveCount(0);
});

test('invalid or failed comments remain editable without undoing a completed upload', async ({ page }) => {
  let fail = true;
  const comments = [];
  await mockLink(page, () => linkInfo(), async (route, pathname) => {
    if (pathname.endsWith('/attempts')) {
      await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size: 0, offset: 0 } });
    } else {
      expect(pathname).toBe(`/api/links/${id}/attempts/${'d'.repeat(32)}/comment`);
      comments.push(route.request().postDataJSON().comment);
      if (fail) {
        await route.fulfill({ status: 401, json: { code: 'session_required', error: 'Session expired.' } });
      } else {
        await route.fulfill({ json: { comment: comments.at(-1) } });
      }
    }
  });
  await page.goto(publicURL);
  await page.locator('input[type=file]').setInputFiles(file('empty.txt'));
  await expect(page.locator('.summary-counts')).toHaveText('1 received · 0 failed · 0 canceled · 0 waiting');
  const comment = page.getByRole('textbox', { name: /Comment/ });
  await comment.fill('é'.repeat(1025));
  await expect(comment).toHaveAttribute('aria-invalid', 'true');
  await expect(page.getByRole('alert')).toContainText('2,048 UTF-8 bytes');
  expect(comments).toEqual([]);
  await expect(page.getByRole('button', { name: 'Clear finished' })).toBeDisabled();
  await comment.fill('Keep this draft');
  await expect(page.getByRole('alert')).toContainText('Comment not saved: Session expired.');
  await expect(comment).toHaveValue('Keep this draft');
  await expect(page.locator('.summary-counts')).toContainText('1 received · 0 failed');
  fail = false;
  await page.getByRole('button', { name: 'Retry comment', exact: true }).click();
  await expect(page.getByText('Comment saved.', { exact: true })).toBeVisible();
  expect(comments).toEqual(['Keep this draft', 'Keep this draft']);
  await expect(page.getByRole('button', { name: 'Clear finished' })).toBeEnabled();
});

for (const count of [1, 2]) {
  test(`${count} completed zero-byte files fill the overall native progressbar`, async ({ page }) => {
    let admissions = 0;
    await mockLink(page, () => linkInfo(), async (route, pathname) => {
      expect(pathname).toBe(`/api/links/${id}/attempts`);
      const request = route.request().postDataJSON();
      expect(request.size).toBe(0);
      admissions++;
      await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size: 0, offset: 0 } });
    });
    await page.goto(publicURL);
    await page.locator('input[type=file]').setInputFiles(Array.from({ length: count }, (_, index) => file(`empty-${index}.txt`)));
    await expect(page.locator('.summary-counts')).toHaveText(`${count} received · 0 failed · 0 canceled · 0 waiting`);
    await expectProgress(page, 1, 1, 100);
    expect(admissions).toBe(count);
    await page.getByRole('button', { name: 'Clear finished', exact: true }).click();
    await expect(page.getByRole('progressbar')).toHaveCount(0);
  });
}

for (const completedFirst of [false, true]) {
  test(`failed and queued zero-byte files do not fill progress${completedFirst ? ' alongside a completed file' : ''}`, async ({ page }) => {
    await mockLink(page, () => linkInfo(), async (route, pathname) => {
      expect(pathname).toBe(`/api/links/${id}/attempts`);
      if (route.request().postDataJSON().name === 'received.txt') {
        await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size: 0, offset: 0 } });
      } else {
        await route.fulfill({ status: 507, json: { code: 'storage_full', error: 'Storage is full.' } });
      }
    });
    await page.goto(publicURL);
    await page.locator('input[type=file]').setInputFiles([
      ...(completedFirst ? [file('received.txt')] : []), file('failed.txt'), file('pending.txt'),
    ]);
    await expect(page.locator('.summary-counts')).toHaveText(`${completedFirst ? 1 : 0} received · 1 failed · 0 canceled · 1 waiting`);
    await expectProgress(page, 0, 1, 0);
    await page.getByRole('button', { name: 'Remove', exact: true }).click();
    await expect(page.locator('.summary-counts')).toHaveText(`${completedFirst ? 1 : 0} received · 1 failed · 0 canceled · 0 waiting`);
    await expectProgress(page, 0, 1, 0);
  });
}

for (const completedFirst of [false, true]) {
  test(`canceled zero-byte files do not fill progress${completedFirst ? ' alongside a completed file' : ''}`, async ({ page }) => {
    let finishAdmission;
    await mockLink(page, () => linkInfo(), async (route, pathname) => {
      if (pathname === `/api/links/${id}/attempts`) {
        if (route.request().postDataJSON().name === 'received.txt') {
          await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size: 0, offset: 0 } });
        } else {
          await new Promise(resolve => { finishAdmission = resolve; });
          await route.fulfill({ json: { id: 'e'.repeat(32), status: 'finalizing', size: 0, offset: 0 } });
        }
      } else {
        expect(pathname).toBe(`/api/links/${id}/attempts/${'e'.repeat(32)}`);
        await route.fulfill({ json: { id: 'e'.repeat(32), status: 'canceled', size: 0, offset: 0 } });
      }
    });
    await page.goto(publicURL);
    const files = [...(completedFirst ? [file('received.txt')] : []), file('cancel.txt')];
    await page.locator('input[type=file]').setInputFiles(files);
    await expect.poll(() => typeof finishAdmission).toBe('function');
    await expectProgress(page, 0, 1, 0);
    await page.getByRole('button', { name: 'Cancel remaining', exact: true }).click();
    finishAdmission();
    await expect(page.locator('.summary-counts')).toHaveText(`${completedFirst ? 1 : 0} received · 0 failed · 1 canceled · 0 waiting`);
    await expectProgress(page, 0, 1, 0);
  });
}

test('mixed zero-byte and nonempty completed files retain byte-based progress', async ({ page }) => {
  await mockLink(page, () => linkInfo(), async (route, pathname) => {
    expect(pathname).toBe(`/api/links/${id}/attempts`);
    const { size } = route.request().postDataJSON();
    await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size, offset: size } });
  });
  await page.goto(publicURL);
  await page.locator('input[type=file]').setInputFiles([file('empty.txt'), file('invoice.txt', 'invoice')]);
  await expect(page.locator('.summary-counts')).toHaveText('2 received · 0 failed · 0 canceled · 0 waiting');
  await expectProgress(page, 7, 7, 100);
});

for (const invalid of ['size', 'filename']) {
  test(`an invalid ${invalid} does not block valid files in the batch`, async ({ page }) => {
    const admitted = [];
    await mockLink(page, () => linkInfo(), async (route, pathname) => {
      expect(pathname).toBe(`/api/links/${id}/attempts`);
      const { name, size } = route.request().postDataJSON();
      admitted.push(name);
      await route.fulfill({ json: { id: 'd'.repeat(32), status: 'completed', size, offset: size } });
    });
    await page.goto(publicURL);
    const badName = invalid === 'filename' ? `${'x'.repeat(256)}.txt` : 'invalid.txt';
    await page.locator('input[type=file]').setInputFiles([
      file('before.txt', 'before'),
      { name: badName, mimeType: 'text/plain', buffer: Buffer.alloc(invalid === 'size' ? 1_000_001 : 1) },
      file('after.txt', 'after'),
    ]);
    const badItem = page.getByRole('listitem').filter({ hasText: badName });
    await expect(page.locator('.summary-counts')).toHaveText('2 received · 1 failed · 0 canceled · 0 waiting');
    await expect(badItem.getByRole('alert')).toContainText(/exceeds/);
    expect(admitted).toEqual(['before.txt', 'after.txt']);
  });
}

for (const theme of ['light', 'dark']) {
  test(`${theme} muted text meets normal-text contrast on every main surface`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: theme });
    await mockLink(page, () => linkInfo());
    await page.goto(publicURL);
    await expect(page.getByRole('heading', { name: 'September invoices' })).toBeVisible();
    await expect(page.locator('html')).toHaveAttribute('data-theme', theme);
    const colors = await page.evaluate(() => {
      const root = getComputedStyle(document.documentElement);
      const footer = document.querySelector('footer');
      return {
        text: getComputedStyle(footer).color,
        fontSize: getComputedStyle(footer).fontSize,
        surfaces: ['--cp-bg', '--cp-bg-elevated', '--cp-surface', '--cp-surface-soft']
          .map(name => ({ name, color: root.getPropertyValue(name).trim() })),
      };
    });
    expect(parseFloat(colors.fontSize)).toBeLessThan(18);
    const text = colors.text.match(/[\d.]+/g).slice(0, 3).map(Number);
    if (theme === 'dark') expect(text).toEqual([176, 176, 176]);
    const luminance = channels => channels.map(value => {
      const c = value / 255;
      return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    }).reduce((sum, c, index) => sum + c * [0.2126, 0.7152, 0.0722][index], 0);
    for (const { name, color } of colors.surfaces) {
      const background = color.slice(1).match(/../g).map(c => parseInt(c, 16));
      const [lighter, darker] = [luminance(text), luminance(background)].sort((a, b) => b - a);
      expect((lighter + 0.05) / (darker + 0.05), `${theme} muted text on ${name}`).toBeGreaterThanOrEqual(4.5);
    }
  });
}
