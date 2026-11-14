// Headless smoke test for the built WebUI bundle.
//
// The bundle is plain JS that talks to the KernelSU manager through the
// `ksu` global. We run it inside linkedom with a fake bridge and assert
// that the cards render the expected text. This catches the class of
// regressions that only show up inside the manager's WebView (a broken
// bridge overload, a truncated command, a mis-parsed status line).
//
// Usage: node test/smoke.mjs [path/to/app.js] [en|zh]

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseHTML } from 'linkedom';

const HERE = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const isLang = (v) => v === 'en' || v === 'zh' || v === 'auto';
const BUNDLE = resolve(
    isLang(argv[0]) || argv[0] === undefined
        ? resolve(HERE, '..', '..', 'webroot', 'app.js')
        : argv[0],
);
const LANG = (isLang(argv[0]) ? argv[0] : argv[1]) ?? 'en';

const STATUS = [
    '2026-11-14T20:00:00+0800 INFO  wg-user: wg-user: running (pid 123)',
    '2026-11-14T20:00:00+0800 INFO  wg-user: last log: DEBUG: keepalive',
    '2026-11-14T20:00:00+0800 INFO  wg-user: transport: 127.0.0.1:51820 (connected)',
    '',
].join('\n');

const respond = (cmd) => {
    if (/wg-user status/.test(cmd)) return STATUS;
    if (/wg-user log/.test(cmd)) return 'DEBUG: handshake ok\nDEBUG: keepalive\n';
    if (/cat .*wireproxy\.conf/.test(cmd)) return 'BindAddress = 127.0.0.1:1080\n[WireGuard]\n';
    return '';
};

const code = readFileSync(BUNDLE, 'utf8');
const { window, document } = parseHTML(
    '<!doctype html><html><body><div id="app"></div></body></html>',
);

const store = new Map();
Object.defineProperty(window, 'localStorage', {
    value: {
        getItem: (k) => (store.has(k) ? store.get(k) : null),
        setItem: (k, v) => store.set(k, String(v)),
    },
});
if (LANG !== 'auto') store.set('wg-user.lang', LANG);

const calls = [];
window.ksu = {
    exec(cmd, opts, cb) {
        calls.push(cmd);
        // KernelSU 3.x: exec(cmd, optionsJson, callbackName)
        if (typeof cb !== 'string') throw new Error('expected the 3-argument exec overload');
        setTimeout(() => window[cb](0, respond(cmd), ''), 1);
    },
    toast() {},
};

globalThis.window = window;
globalThis.document = document;

new Function(code)();

await new Promise((r) => setTimeout(r, 150));

const html = document.getElementById('app').innerHTML;

const checks = [
    ['green status dot', html.includes('bg-emerald-400')],
    ['status label', /Running|运行中/.test(html)],
    ['status detail', html.includes('wg-user: running (pid 123)')],
    ['status detail includes transport', html.includes('transport: 127.0.0.1:51820 (connected)')],
    ['transport state label', /Connected|已连接/.test(html)],
    ['status detail hides the log tail', !html.includes('last log:')],
    ['no tunnel leftovers in rendered UI', !/tunnel/i.test(html)],
    ['log card has content', html.includes('DEBUG: handshake ok')],
    ['the 3-arg exec overload is used', calls.length > 0],
    ['commands use the absolute module path', calls.every((c) => c.startsWith('/data/adb/modules/wg-user/'))],
];

let failed = 0;
for (const [name, ok] of checks) {
    if (!ok) failed++;
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}`);
}
console.log(`--- ${calls.length} exec calls, lang=${LANG}`);

if (failed) {
    console.log(html.slice(0, 2000));
}
process.exit(failed ? 1 : 0);
