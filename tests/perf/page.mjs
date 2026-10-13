// Drives catalog.html in headless Chrome and times what a person waits for. No packages needed:
// Node 22+ talks to Chrome over the DevTools protocol with its built-in WebSocket.
//
//   node tests/perf/page.mjs --url http://127.0.0.1:8765/catalog.html --chrome "/path/to/Chrome" --out page.json
//
// Run by tests/perf_test.go (TestPerf), which builds the fake disk and starts the server. Every
// measure is in milliseconds, except page.sql_per_click_max and page.fullscans_click_max: how many SQL
// statements, and how many full scans of the files table, the worst click ran (a click should run
// none: the view is redrawn from what the page already read), and page.fullscans_folder_max: full
// scans while drawing a folder (a folder should cost what it holds, not the whole disk).
import { spawn } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const arg = k => { const i = process.argv.indexOf('--' + k); return i > 0 ? process.argv[i + 1] : ''; };
const url = arg('url'), chromePath = arg('chrome'), outPath = arg('out');
if (!url || !chromePath) { console.error('usage: page.mjs --url URL --chrome PATH [--out FILE]'); process.exit(2); }
const sleep = ms => new Promise(r => setTimeout(r, ms));

const profile = mkdtempSync(join(tmpdir(), 'sidecar-perf-chrome-'));
const chrome = spawn(chromePath, ['--headless=new', '--remote-debugging-port=0', '--user-data-dir=' + profile,
  '--no-first-run', '--no-default-browser-check', '--disable-extensions', '--window-size=1400,1000', 'about:blank'],
  { stdio: 'ignore' });
let ws, seq = 0;
const pending = new Map(), errors = [];
function cleanup() { try { ws && ws.close(); } catch (e) {} chrome.kill(); try { rmSync(profile, { recursive: true, force: true }); } catch (e) {} }
process.on('exit', cleanup);

async function connect() {
  const f = join(profile, 'DevToolsActivePort');
  for (let i = 0; i < 200 && !existsSync(f); i++) await sleep(50);
  const [port, path] = readFileSync(f, 'utf8').trim().split('\n');
  ws = new WebSocket(`ws://127.0.0.1:${port}${path}`);
  await new Promise((ok, bad) => { ws.onopen = ok; ws.onerror = bad; });
  ws.onmessage = e => {
    const m = JSON.parse(e.data);
    if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.bad(new Error(m.error.message)) : p.ok(m.result); }
    else if (m.method === 'Runtime.exceptionThrown') errors.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
  };
}
function send(method, params = {}, sessionId) {
  const id = ++seq;
  ws.send(JSON.stringify({ id, method, params, sessionId }));
  return new Promise((ok, bad) => pending.set(id, { ok, bad }));
}
let sid;
async function evaluate(expr) {
  const r = await send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true }, sid);
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
}

// installed in the page: times a step, counts the SQL it ran and how much of it scans every file
const harness = `
window.__perf = { log: [], inflight: 0 };
{
  const origQ = q;
  const cached = (sql, params) => typeof qcache !== 'undefined' && qcache.has(sql + '\u0000' + JSON.stringify(params));
  q = function (sql, params = []) {
    if (cached(sql, params)) return origQ(sql, params); // answered from the page's memory: no SQL ran
    const t0 = performance.now(); const r = origQ(sql, params); __perf.log.push({ sql, params, ms: performance.now() - t0 }); return r;
  };
  const origFetch = window.fetch;
  window.fetch = function (...a) { __perf.inflight++; return origFetch.apply(this, a).finally(() => __perf.inflight--); };
}
window.__idle = async () => { for (let i = 0; i < 600 && __perf.inflight; i++) await new Promise(r => setTimeout(r, 25)); };
window.__fullScans = () => {
  let n = 0;
  for (const e of __perf.log) {
    let plan; try { plan = db.exec('EXPLAIN QUERY PLAN ' + e.sql, e.params); } catch (x) { continue; }
    // a scan of the table, or an index search narrowed only by disk, reads every file of that disk
    for (const row of (plan[0] || { values: [] }).values)
      if (/^SCAN (files|f)\\b|^SEARCH (files|f) USING (COVERING )?INDEX \\w+ \\(source_id=\\?\\)$/.test(String(row[row.length - 1]))) n++;
  }
  return n;
};
window.__measure = async (fn) => {
  await __idle();
  __perf.log = [];
  const t0 = performance.now();
  await fn();
  document.body.getBoundingClientRect(); void document.body.offsetHeight; // the layout the click causes
  const ms = performance.now() - t0;
  const out = { ms, sql: __perf.log.length, scans: __fullScans(), slow: __perf.log.slice().sort((a, b) => b.ms - a.ms).slice(0, 3).map(e => e.ms.toFixed(1) + 'ms ' + e.sql.replace(/\\s+/g, ' ').slice(0, 90)) };
  await __idle();
  return out;
};
true`;

const results = {};
let worstSql = 0, worstScans = 0;
// the speed must not come from skipping the work: what the person clicked is on screen
async function expect(expr, what) { if (!(await evaluate(expr))) throw new Error('wrong page: ' + what); }
let folderScans = 0;
async function step(name, body, isClick = false, inFolder = false) {
  const r = await evaluate(`__measure(async () => { ${body} })`);
  results['page.' + name] = +r.ms.toFixed(1);
  if (isClick) { worstSql = Math.max(worstSql, r.sql); worstScans = Math.max(worstScans, r.scans); }
  if (inFolder) folderScans = Math.max(folderScans, r.scans);
  console.log(`  ${name.padEnd(22)} ${r.ms.toFixed(1).padStart(8)} ms  ${String(r.sql).padStart(4)} sql  ${String(r.scans).padStart(3)} full scans` +
    (r.slow.length ? '\n' + r.slow.map(s => '      ' + s).join('\n') : ''));
}

try {
  await connect();
  const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
  ({ sessionId: sid } = await send('Target.attachToTarget', { targetId, flatten: true }));
  await send('Runtime.enable', {}, sid);
  await send('Page.enable', {}, sid);
  await send('Page.navigate', { url }, sid);
  let loaded = 0;
  for (let i = 0; i < 2400; i++) { // up to 2 minutes
    await sleep(50);
    try { loaded = await evaluate(`(typeof db !== 'undefined' && db && actions && document.querySelector('#listing tr')) ? performance.now() : 0`); } catch (e) {}
    if (loaded) break;
  }
  if (!loaded) throw new Error('the page did not load' + (errors.length ? ': ' + errors.join('; ') : ''));
  results['page.load'] = +loaded.toFixed(1);
  console.log(`  ${'load'.padEnd(22)} ${loaded.toFixed(1).padStart(8)} ms`);
  await evaluate(harness);

  const top = `state.view = 'folders'; state.source = String(sources[0].id); state.kind = 'all';`;
  const click = (sel, root = '#listing') => `const b = document.querySelector('${root} ${sel}'); if (!b) throw new Error('no ${sel}'); b.click();`;
  await step('render_map_root', `${top} state.prefix = ''; renderMap();`);
  await step('click_mark_root', click('button[data-m=delete]'), true);
  await step('render_map_folder', `${top} state.prefix = 'Photos/2004'; renderMap();`, false, true);
  await step('click_mark_folder', click('button[data-m=keep]'), true);
  await step('view_files', `${top} state.prefix = 'Photos'; state.view = 'files'; renderMap();`, false, true);
  await step('click_mark_files', click('button[data-m=review]'), true);
  await expect(`document.querySelectorAll('#listing tr.m-review').length >= 1`, 'a file answered Not sure shows it');
  await evaluate(`(() => { const c = document.querySelector('#listing input.selall'); c.checked = true; c.dispatchEvent(new Event('change', { bubbles: true })); return sel.size; })()`);
  await step('bulk_mark_300', click('button[data-b=delete]', '#selbar'), true);
  await expect(`document.querySelectorAll('#listing tr.m-delete').length >= 300`, 'every ticked file shows Let go');
  await step('view_photos', `${top} state.prefix = 'Photos'; state.view = 'photos'; renderMap();`, false, true);
  await evaluate(`(() => { const f = document.querySelector('#gallery .pt'); f.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 50, clientY: 50 })); return true; })()`);
  await step('ctx_mark_photo', click('button[data-a=delete]', '#ctx'), true);
  await expect(`!!document.querySelector('#gallery .pt.m-delete')`, 'the photo shows Let go');
  await evaluate(`state.view = 'folders'; true`);
  await step('tab_dups', click('button[data-tab=dups]', '#nav'));
  await step('tab_marks', click('button[data-tab=marks]', '#nav'));
  await expect(`document.querySelectorAll('#marklist tr').length > 300`, 'the tidying piles list the answers');
  await step('search_name', `document.querySelector('#nav button[data-tab=search]').click(); $('#q').value = 'invoice'; await runSearch();`);
  await step('reload_after_mark', `await reloadDb('map');`);
  await expect(`(async () => { const p = loading(sleep(500)); await sleep(350); const on = $('#loading').classList.contains('on'); await p; return on && !$('#loading').classList.contains('on'); })()`,
    'the loading bar shows during a slow request and hides after');
  results['page.sql_per_click_max'] = worstSql;
  results['page.fullscans_click_max'] = worstScans;
  results['page.fullscans_folder_max'] = folderScans;
  if (errors.length) throw new Error('errors in the page: ' + errors.join('; '));
  if (outPath) writeFileSync(outPath, JSON.stringify(results, null, 2));
  cleanup();
  process.exit(0);
} catch (e) {
  console.error('page.mjs:', e.message);
  cleanup();
  process.exit(1);
}
