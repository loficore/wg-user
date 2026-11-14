// Headless smoke test for the built WebUI bundle.
//
// The bundle is plain JS that talks to the KernelSU manager through the
// `ksu` global. We run it inside linkedom with a fake bridge and assert
// that the cards render the expected text. This catches the class of
// regressions that only show up inside the manager's WebView (a broken
// bridge overload, a truncated command, a mis-parsed status line).
//
// Two renders are covered: a running daemon with a connected transport and a
// stopped daemon, which has no transport line at all and must show Off.
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

const STATUS_RUNNING = [
    '2026-11-14T20:00:00+0800 INFO  wg-user: wg-user: running (pid 123)',
    '2026-11-14T20:00:00+0800 INFO  wg-user: last log: DEBUG: keepalive',
    '2026-11-14T20:00:00+0800 INFO  wg-user: transport: 127.0.0.1:51820 (connected)',
    '',
].join('\n');

// `wg-user status` prints this and exits 1; the bundle appends `|| true`, so
// the bridge callback still reports success (code 0) with the output.
const STATUS_STOPPED = ['2026-11-14T20:00:00+0800 INFO  wg-user: wg-user: stopped', ''].join('\n');

// off mode: the daemon runs but has no transport, so it reports `off` (Fix B).
const STATUS_OFF_MODE = [
    '2026-11-14T20:00:00+0800 INFO  wg-user: wg-user: running (pid 123)',
    '2026-11-14T20:00:00+0800 INFO  wg-user: transport: udp (off)',
    '',
].join('\n');

const respond = (cmd, status) => {
    if (/wg-user status/.test(cmd)) return status;
    if (/wg-user log/.test(cmd)) return 'DEBUG: handshake ok\nDEBUG: keepalive\n';
    if (/cat .*wireproxy\.conf/.test(cmd)) return 'BindAddress = 127.0.0.1:1080\n[WireGuard]\n';
    return '';
};

const code = readFileSync(BUNDLE, 'utf8');

// Mount the bundle into a fresh document with a fake KSU bridge and return the
// rendered HTML plus the commands the app issued.
const render = async (status) => {
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
            setTimeout(() => window[cb](0, respond(cmd, status), ''), 1);
        },
        toast() {},
    };

    globalThis.window = window;
    globalThis.document = document;

    new Function(code)();

    await new Promise((r) => setTimeout(r, 150));
    return { html: document.getElementById('app').innerHTML, calls };
};

// The status card's Transport row: from its label to the end of the row's div.
const transportRow = (html) => {
    const at = html.search(/>(Transport|传输)<\/span>/);
    if (at < 0) return '';
    return html.slice(at, html.indexOf('</div>', at));
};

const running = await render(STATUS_RUNNING);
const offMode = await render(STATUS_OFF_MODE);
const stopped = await render(STATUS_STOPPED);

const checks = [
    ['green status dot', running.html.includes('bg-emerald-400')],
    ['status label', /Running|运行中/.test(running.html)],
    ['status detail', running.html.includes('wg-user: running (pid 123)')],
    ['transport row shows the detail', running.html.includes('127.0.0.1:51820')],
    ['transport row shows the state', /Connected|已连接/.test(transportRow(running.html))],
    [
        'status detail does not repeat the transport line',
        !running.html.includes('transport: 127.0.0.1:51820'),
    ],
    ['status detail hides the log tail', !running.html.includes('last log:')],
    ['no tunnel leftovers in rendered UI', !/tunnel/i.test(running.html)],
    ['log card has content', running.html.includes('DEBUG: handshake ok')],
    ['the 3-arg exec overload is used', running.calls.length > 0],
    [
        'commands use the absolute module path',
        running.calls.every((c) => c.startsWith('/data/adb/modules/wg-user/')),
    ],

    ['stopped: red status dot', stopped.html.includes('bg-rose-400')],
    ['stopped: status label', /Stopped|已停止/.test(stopped.html)],
    ['stopped: meta shows the status line', stopped.html.includes('wg-user: stopped')],
    ['stopped: transport row shows off', /Off|未启用/.test(transportRow(stopped.html))],
    ['stopped: transport is not unknown', !/Unknown|未知/.test(transportRow(stopped.html))],
    [
        'stopped: commands use the absolute module path',
        stopped.calls.every((c) => c.startsWith('/data/adb/modules/wg-user/')),
    ],

    ['off mode: status label', /Running|运行中/.test(offMode.html)],
    ['off mode: transport row shows off', /Off|未启用/.test(transportRow(offMode.html))],
    ['off mode: transport is not unknown', !/Unknown|未知/.test(transportRow(offMode.html))],
];

let failed = 0;
for (const [name, ok] of checks) {
    if (!ok) failed++;
    console.log(`${ok ? 'ok  ' : 'FAIL'} ${name}`);
}
const calls = running.calls.length + offMode.calls.length + stopped.calls.length;
console.log(`--- ${calls} exec calls over 3 renders, lang=${LANG}`);

if (failed) {
    console.log(`\n--- running render ---\n${running.html.slice(0, 2000)}`);
    console.log(`\n--- off-mode render ---\n${offMode.html.slice(0, 2000)}`);
    console.log(`\n--- stopped render ---\n${stopped.html.slice(0, 2000)}`);
}
process.exit(failed ? 1 : 0);
