// Screenshot the stories of one package and collect console errors.
// usage: node shot.js PORT PKG OUT_DIR BASELINE_DIR KEEP
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

(async () => {
  const [port, pkg, out, base, keep] = process.argv.slice(2);
  const origin = `http://127.0.0.1:${port}`;
  const log = (m) => console.error(`[${new Date().toISOString().slice(11, 19)}] ${m}`);
  // Nothing here may wait for ever: a stuck check must fail, not hang.
  setTimeout(() => { log('gave up after 8 minutes'); process.exit(1); }, 8 * 60000).unref();
  log(`reading the story index on port ${port}`);
  const index = await (await fetch(`${origin}/index.json`, { signal: AbortSignal.timeout(120000) })).json();
  const ids = Object.values(index.entries)
    .filter((e) => e.type === 'story' && e.importPath.includes(`/packages/react/${pkg}/`))
    .map((e) => e.id)
    .slice(0, Number(process.env.MAX_STORIES || 6));
  fs.mkdirSync(out, { recursive: true });
  log(`${ids.length} stories; launching the browser`);
  // Chromium's sandbox needs user namespaces, which a container on a host
  // that restricts them (Ubuntu 24.04 and later) does not get: Chromium
  // then exits at once with "No usable sandbox". Playwright's own launch
  // turns the sandbox off for this reason; the detached browser below must
  // do the same.
  const sandbox = ['--no-sandbox'];
  let browser;
  if (keep === '1') {
    // G2: a browser the agent starts and then walks away from. It is its
    // own detached process, so it outlives this script; Playwright's own
    // launch would close it when the script exits.
    // Each check starts its own browser, on its own port and profile: one
    // left over from an earlier check would serve pages from its cache.
    const debugPort = 20000 + (process.pid % 20000);
    const { spawn } = require('child_process');
    spawn(chromium.executablePath(), ['--headless=new', `--remote-debugging-port=${debugPort}`,
      `--user-data-dir=${path.join(out, 'chrome-profile-' + process.pid)}`, '--no-first-run', '--disable-dev-shm-usage', '--disable-gpu', ...sandbox, 'about:blank'],
      { detached: true, stdio: 'ignore' }).unref();
    for (let i = 0; ; i++) {
      try {
        browser = await chromium.connectOverCDP(`http://127.0.0.1:${debugPort}`, { timeout: 5000 });
        break;
      } catch (e) {
        if (i === 0) log(`waiting for the browser on port ${debugPort}`);
        if (i >= 60) { log(`the browser never answered on port ${debugPort}`); throw e; }
        await new Promise((r) => setTimeout(r, 500));
      }
    }
  } else {
    browser = await chromium.launch({ timeout: 120000 });
  }
  let errors = 0, changed = 0;
  const messages = [];
  for (const id of ids) {
    // After a fix the dev server rebuilds, and a page loaded mid-build
    // reports missing chunks. Give each story three attempts and count only
    // the last one.
    let found = [];
    for (let attempt = 1; attempt <= 3; attempt++) {
      found = [];
      log(`${id}, attempt ${attempt}`);
      const page = await browser.newPage();
      page.setDefaultTimeout(60000);
      const note = (m) => found.push(String(m).slice(0, 200));
      // A missing hot-update manifest is the dev server's hot reload
      // polling, not an error of the story.
      const hot = (url) => /\.hot-update\./.test(url || '');
      page.on('console', (m) => { if (m.type() === 'error' && !hot(m.location().url)) note(m.text()); });
      page.on('pageerror', note);
      page.on('response', (r) => { if (r.status() >= 400 && !hot(r.url())) log(`HTTP ${r.status()} ${r.url()}`); });
      // The dev server holds connections open, so neither "load" nor
      // "networkidle" is reliable. Wait for the story itself instead.
      await page.goto(`${origin}/iframe.html?id=${id}&viewMode=story`, { waitUntil: 'domcontentloaded', timeout: 60000 }).catch(() => note(`story ${id} did not load`));
      await page.waitForSelector('#storybook-root > *, body.sb-show-errordisplay', { state: 'attached', timeout: 20000 }).catch(() => note(`story ${id} did not render`));
      await page.waitForTimeout(300);
      const file = path.join(out, `${id}.png`);
      await page.screenshot({ path: file }).catch(() => note(`story ${id} could not be captured`));
      // Only a page caught mid-build is worth another attempt; a console
      // error from the story itself will not go away.
      const midBuild = found.some((m) => /Failed to load resource|ChunkLoadError|did not (load|render)/.test(m));
      const done = !midBuild || attempt === 3;
      if (done) {
        const ref = path.join(base, `${id}.png`);
        if (fs.existsSync(ref) && fs.existsSync(file) && !fs.readFileSync(ref).equals(fs.readFileSync(file))) changed++;
      }
      // G2 leaves the last page of each story open, with the browser.
      if (!done || keep !== '1') await page.close();
      if (done) break;
      await new Promise((r) => setTimeout(r, 5000));
    }
    errors += found.length;
    for (const m of found) if (messages.length < 5) messages.push(m);
    if (found.length > 0) break; // one broken story fails the package; skip the rest
  }
  console.log(JSON.stringify({ pkg, stories: ids.length, errors, changed, messages }));
  // G2: the agent walks away without closing the browser.
  if (keep === '1') process.exit(0);
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
