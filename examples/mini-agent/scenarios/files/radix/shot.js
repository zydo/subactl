// Screenshot the stories of one package and collect console errors.
// usage: node shot.js PORT PKG OUT_DIR BASELINE_DIR KEEP
const { chromium } = require('playwright');
const fs = require('fs');
const path = require('path');

(async () => {
  const [port, pkg, out, base, keep] = process.argv.slice(2);
  const origin = `http://127.0.0.1:${port}`;
  const index = await (await fetch(`${origin}/index.json`)).json();
  const ids = Object.values(index.entries)
    .filter((e) => e.type === 'story' && e.importPath.includes(`/packages/react/${pkg}/`))
    .map((e) => e.id)
    .slice(0, Number(process.env.MAX_STORIES || 6));
  fs.mkdirSync(out, { recursive: true });
  // As root (in a container) Chromium cannot use its sandbox.
  const browser = await chromium.launch({ args: process.getuid() === 0 ? ['--no-sandbox'] : [] });
  let errors = 0, changed = 0;
  const messages = [];
  for (const id of ids) {
    const page = await browser.newPage();
    const note = (m) => { errors++; if (messages.length < 5) messages.push(String(m).slice(0, 200)); };
    page.on('console', (m) => { if (m.type() === 'error') note(m.text()); });
    page.on('pageerror', note);
    // The dev server keeps a connection open, so "networkidle" never comes.
    await page.goto(`${origin}/iframe.html?id=${id}&viewMode=story`, { waitUntil: 'load', timeout: 120000 });
    await page.waitForSelector('#storybook-root > *, .sb-show-errordisplay', { timeout: 120000 }).catch(() => note(`story ${id} did not render`));
    await page.waitForTimeout(300);
    const file = path.join(out, `${id}.png`);
    await page.screenshot({ path: file });
    const ref = path.join(base, `${id}.png`);
    if (fs.existsSync(ref) && !fs.readFileSync(ref).equals(fs.readFileSync(file))) changed++;
    if (keep !== '1') await page.close();
  }
  console.log(JSON.stringify({ pkg, stories: ids.length, errors, changed, messages }));
  // G2: the agent walks away without closing the browser.
  if (keep === '1') { browser.process().unref(); process.exit(0); }
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
