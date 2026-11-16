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
  // As root (in a container) Chromium cannot use its sandbox.
  log(`${ids.length} stories; launching the browser`);
  const browser = await chromium.launch({ args: process.getuid() === 0 ? ['--no-sandbox'] : [], timeout: 120000 });
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
  if (keep === '1') { browser.process().unref(); process.exit(0); }
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
