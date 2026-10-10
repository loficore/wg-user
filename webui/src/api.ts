// KernelSU WebUI bridge.
//
// The `ksu` object is injected with addJavascriptInterface; its overloads differ
// between manager releases. Verified against me.weishu.kernelsu v3.3.1 by
// decompiling the manager APK (WebViewInterface):
//
//   exec(String) -> String                 synchronous, returns THE LAST LINE ONLY
//   exec(String, String) -> void           calls the 3-arg form with a null callback
//   exec(String, String, String) -> void   runs the command, then evaluates
//                                          window[cbName](exitCode, stdout, stderr)
//                                          with stdout/stderr joined by "\n"
//
// Only the 3-argument form returns complete output, so it is tried first.
// `ksu.readAssets` no longer exists; module files are read with `cat`.

export const MODULE_DIR = '/data/adb/modules/wg-user';

const TIMEOUT_MS = 30_000;

interface KsuBridge {
    exec?: (...args: unknown[]) => unknown;
    toast?: (message: string) => void;
}

declare global {
    interface Window {
        ksu?: KsuBridge;
        [key: string]: unknown;
    }
}

type ExecCallback = (errno: number, stdout?: string, stderr?: string) => void;

const bridge = (): KsuBridge | null => {
    if (typeof window === 'undefined') return null;
    const k = window.ksu;
    return k && typeof k === 'object' ? k : null;
};

export const ksuExec = (cmd: string): Promise<string> =>
    new Promise<string>((resolve, reject) => {
        const k = bridge();
        if (!k || typeof k.exec !== 'function') {
            reject(new Error('ksu.exec unavailable'));
            return;
        }

        let settled = false;
        let timer: ReturnType<typeof setTimeout> | null = null;

        const finish = (fn: (value: string) => void, value: string): void => {
            if (settled) return;
            settled = true;
            if (timer !== null) clearTimeout(timer);
            timer = null;
            fn(value);
        };
        const ok = (value: unknown): void => finish(resolve, String(value ?? ''));
        const rejectWith = (value: unknown): void => {
            if (settled) return;
            settled = true;
            if (timer !== null) clearTimeout(timer);
            timer = null;
            reject(value instanceof Error ? value : new Error(String(value)));
        };

        const settle = (errno: number, stdout?: string, stderr?: string): void => {
            const out = [stdout, stderr].filter((v) => v).join('\n');
            if (errno) rejectWith((stderr || stdout || `exit ${errno}`).trim());
            else ok(out);
        };

        // ---- 1) KernelSU 3.x: exec(cmd, optionsJson, callbackName) ----
        const cbName = `__wgu_${Date.now()}_${Math.random().toString(36).slice(2)}`;
        const store = window as unknown as Record<string, unknown>;
        const drop = (): void => {
            delete store[cbName];
        };

        store[cbName] = ((errno, stdout, stderr) => {
            drop();
            settle(Number(errno) || 0, stdout, stderr);
        }) satisfies ExecCallback;

        try {
            k.exec(cmd, '{}', cbName);
            timer = setTimeout(() => {
                drop();
                rejectWith(new Error('ksu bridge timeout'));
            }, TIMEOUT_MS);
            return;
        } catch {
            drop();
        }

        // ---- 2) synchronous one-arg form (last line only) or a promise ----
        try {
            const r = k.exec(cmd);
            if (typeof r === 'string') {
                ok(r);
                return;
            }
            if (r && typeof (r as Promise<unknown>).then === 'function') {
                void (r as Promise<unknown>).then(
                    (v) =>
                        v && typeof v === 'object'
                            ? settle(
                                  Number((v as { errno?: number }).errno) || 0,
                                  (v as { stdout?: string }).stdout,
                                  (v as { stderr?: string }).stderr,
                              )
                            : ok(v),
                    rejectWith,
                );
                return;
            }
        } catch {
            /* fall through */
        }

        // ---- 3) very old: exec(cmd, cb(out, err)) ----
        timer = setTimeout(() => rejectWith(new Error('ksu bridge timeout')), TIMEOUT_MS);
        try {
            k.exec(cmd, (out: unknown, err: unknown) => (err ? rejectWith(err) : ok(out)));
        } catch (e) {
            rejectWith(e);
        }
    });

/** Read a module-relative file, e.g. `etc/wireproxy.conf`. */
export const ksuReadFile = (rel: string): Promise<string> =>
    ksuExec(`cat ${MODULE_DIR}/${rel} 2>&1`);

/** Best-effort toast; silently ignored on managers without the API. */
export const ksuToast = (message: string): void => {
    try {
        const k = bridge();
        if (k && typeof k.toast === 'function') k.toast(message);
    } catch {
        /* ignore */
    }
};
