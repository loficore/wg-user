// build.mjs — bundle the Preact app with esbuild and compile Tailwind CSS.
// Outputs to ../webroot/ (the KSU WebUI root).

import { build, context } from 'esbuild';
import { execFileSync } from 'node:child_process';
import { copyFileSync, mkdirSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SRC_DIR = __dirname;
const OUT_DIR = resolve(__dirname, '..', 'webroot');
const isWatch = process.argv.includes('--watch');

function runTailwind() {
    const tw = resolve(SRC_DIR, 'node_modules', '.bin', 'tailwindcss');
    execFileSync(tw, [
        '-c', resolve(SRC_DIR, 'tailwind.config.js'),
        '-i', resolve(SRC_DIR, 'src', 'input.css'),
        '-o', resolve(OUT_DIR, 'style.css'),
        '--minify',
    ], { stdio: 'inherit' });
}

function copyHtml() {
    copyFileSync(
        resolve(SRC_DIR, 'src', 'index.html'),
        resolve(OUT_DIR, 'index.html'),
    );
}

async function bundle() {
    mkdirSync(OUT_DIR, { recursive: true });
    runTailwind();
    copyHtml();
    const opts = {
        entryPoints: [resolve(SRC_DIR, 'src', 'main.tsx')],
        bundle: true,
        minify: !isWatch,
        sourcemap: isWatch ? 'inline' : false,
        target: ['es2020'],
        format: 'iife',
        jsx: 'automatic',
        jsxImportSource: 'preact',
        loader: { '.ts': 'ts', '.tsx': 'tsx' },
        outfile: resolve(OUT_DIR, 'app.js'),
        logLevel: 'info',
        define: { 'process.env.NODE_ENV': isWatch ? '"development"' : '"production"' },
    };
    if (isWatch) {
        const ctx = await context(opts);
        await ctx.watch();
        console.log('webui: watching for changes…');
    } else {
        await build(opts);
        console.log('webui: built.');
    }
}

bundle().catch((e) => {
    console.error('build failed:', e);
    process.exit(1);
});
