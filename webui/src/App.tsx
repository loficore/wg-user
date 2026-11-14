import type { ComponentChildren } from 'preact';
import { useCallback, useEffect, useState } from 'preact/hooks';
import { ksuExec, ksuReadFile, ksuToast, MODULE_DIR as MOD } from './api';
import { LANGS, setLang, t, useLang } from './i18n';

// ---- shared components ----

const Dot = ({ ok, err }: { ok?: boolean; err?: boolean }) => (
    <span
        class={`inline-block w-2.5 h-2.5 rounded-full mr-2 align-middle ${
            ok ? 'bg-emerald-400 shadow-[0_0_6px_#34d399]' : err ? 'bg-rose-400' : 'bg-slate-500'
        }`}
    />
);

const Card = ({
    title,
    sub,
    children,
}: {
    title: string;
    sub?: string;
    children: ComponentChildren;
}) => (
    <section class="bg-slate-900 border border-slate-800 rounded-lg p-4 mb-4">
        <h2 class="text-base font-semibold mb-2.5">
            {title}
            {sub && <small class="text-slate-400 font-normal ml-2 text-xs">{sub}</small>}
        </h2>
        {children}
    </section>
);

const Button = ({
    onClick,
    children,
    primary = false,
    disabled = false,
    id,
}: {
    onClick: () => void;
    children: ComponentChildren;
    primary?: boolean;
    disabled?: boolean;
    id?: string;
}) => (
    <button
        id={id}
        onClick={onClick}
        disabled={disabled}
        class={`px-3.5 py-1.5 text-sm rounded-md border transition ${
            primary
                ? 'bg-indigo-500 border-indigo-500 hover:bg-indigo-400 text-white'
                : 'bg-slate-800 border-slate-700 hover:bg-slate-700'
        } disabled:opacity-50 disabled:cursor-not-allowed`}
    >
        {children}
    </button>
);

const errorText = (e: unknown): string =>
    String((e as { message?: string } | null)?.message ?? e).slice(0, 300);

const useDaemonCommand = (subcommand: string, onChange?: () => void) => {
    const [busy, setBusy] = useState(false);
    const run = useCallback(async () => {
        setBusy(true);
        try {
            await ksuExec(`${MOD}/wg-user ${subcommand} 2>&1`);
        } catch (e) {
            ksuToast(t('toast.failed', { cmd: subcommand, msg: errorText(e) }));
            console.error(e);
        }
        setBusy(false);
        if (onChange) onChange();
    }, [subcommand, onChange]);
    return { busy, run };
};

// ---- sections ----

const LangSwitch = () => {
    const lang = useLang();
    return (
        <div class="flex items-center gap-1" aria-label={t('app.language')}>
            {LANGS.map((item) => (
                <button
                    key={item.id}
                    onClick={() => setLang(item.id)}
                    class={`px-2 py-0.5 text-xs rounded border transition ${
                        item.id === lang
                            ? 'bg-slate-700 border-slate-600 text-white'
                            : 'bg-slate-900 border-slate-800 text-slate-400 hover:text-slate-200'
                    }`}
                >
                    {item.label}
                </button>
            ))}
        </div>
    );
};

const Header = () => (
    <header class="mb-4 flex items-start justify-between gap-3">
        <div>
            <h1 class="text-2xl font-bold">wg-user</h1>
            <p class="text-slate-400 text-sm">{t('app.subtitle')}</p>
        </div>
        <LangSwitch />
    </header>
);

type TransportState = 'connected' | 'reconnecting' | 'off' | 'unknown';

const TRANSPORT_STATES: readonly TransportState[] = ['connected', 'reconnecting', 'off', 'unknown'];

// `status` prints `transport: <detail> (<state>)`; <detail> may be empty or contain brackets.
const parseTransport = (lines: string[]): TransportState => {
    const line = lines.find((l) => /^transport:\s*/.test(l));
    const m = line?.match(/\((\w+)\)\s*$/);
    const state = m?.[1];
    return TRANSPORT_STATES.find((s) => s === state) ?? 'unknown';
};

const transportLabel = (state: TransportState | null): string => {
    switch (state) {
        case 'connected':
            return t('transport.connected');
        case 'reconnecting':
            return t('transport.reconnecting');
        case 'off':
            return t('transport.off');
        case 'unknown':
            return t('transport.unknown');
        default:
            return t('status.checking');
    }
};

const Status = ({ refreshKey }: { refreshKey: number }) => {
    const [state, setState] = useState<'ok' | 'err' | 'loading'>('loading');
    const [meta, setMeta] = useState('');
    const [transport, setTransport] = useState<TransportState | null>(null);

    useEffect(() => {
        let cancelled = false;
        const tick = async (): Promise<void> => {
            try {
                const out = await ksuExec(`${MOD}/wg-user status 2>&1 || true`);
                if (cancelled) return;
                const running = /wg-user:\s*running/.test(out);
                const lines = out
                    .split('\n')
                    .map((line) => line.replace(/^\S+\s+INFO\s+wg-user:\s*/, '').trim())
                    .filter((line) => line !== '');
                setState(running ? 'ok' : 'err');
                // Stopped means no transport line is printed; the transport is off.
                setTransport(running ? parseTransport(lines) : 'off');
                setMeta(lines.filter((line) => !/^last log:/.test(line)).join(' · '));
            } catch (e) {
                if (cancelled) return;
                setState('err');
                setTransport('unknown');
                setMeta(errorText(e));
            }
        };
        void tick();
        const id = setInterval(() => void tick(), 5000);
        return () => {
            cancelled = true;
            clearInterval(id);
        };
    }, [refreshKey]);

    const label =
        state === 'ok'
            ? t('status.running')
            : state === 'err'
              ? t('status.stopped')
              : t('status.checking');

    return (
        <Card title={t('status.title')}>
            <div class="flex items-center">
                <Dot ok={state === 'ok'} err={state === 'err'} />
                <span>{label}</span>
            </div>
            <div class="flex items-center text-xs mt-2">
                <span class="text-slate-400 mr-2">{t('transport.label')}</span>
                <span>{transportLabel(transport)}</span>
            </div>
            <div class="text-slate-400 text-xs mt-1">{meta}</div>
        </Card>
    );
};

const Controls = ({ onChange }: { onChange: () => void }) => {
    const start = useDaemonCommand('start', onChange);
    const stop = useDaemonCommand('stop', onChange);
    const restart = useDaemonCommand('restart', onChange);
    const reload = useDaemonCommand('reload', onChange);
    const anyBusy = start.busy || stop.busy || restart.busy || reload.busy;
    return (
        <Card title={t('controls.title')}>
            <div class="flex flex-wrap gap-2">
                <Button id="btn-start" disabled={anyBusy} onClick={start.run}>
                    {t('controls.start')}
                </Button>
                <Button id="btn-stop" disabled={anyBusy} onClick={stop.run}>
                    {t('controls.stop')}
                </Button>
                <Button id="btn-restart" disabled={anyBusy} onClick={restart.run}>
                    {t('controls.restart')}
                </Button>
                <Button id="btn-reload" disabled={anyBusy} onClick={reload.run}>
                    {t('controls.reload')}
                </Button>
            </div>
        </Card>
    );
};

const Config = ({ onChange }: { onChange: () => void }) => {
    const [editor, setEditor] = useState('');
    const [status, setStatus] = useState('');

    const load = useCallback(async () => {
        try {
            setEditor(await ksuReadFile('etc/wireproxy.conf'));
            setStatus(t('config.loaded'));
        } catch (e) {
            setStatus(t('config.loadError', { msg: errorText(e) }));
        }
    }, []);

    const save = useCallback(async () => {
        const stage = `${MOD}/.save.wireproxy.conf.tmp`;
        const b64 = btoa(unescape(encodeURIComponent(editor)));
        try {
            await ksuExec(
                `echo '${b64}' | base64 -d > ${stage} && ${MOD}/wg-user save-config ${stage} 2>&1 && rm ${stage}`,
            );
            setStatus(t('config.saved'));
            onChange();
        } catch (e) {
            setStatus(t('config.saveError', { msg: errorText(e) }));
        }
    }, [editor, onChange]);

    return (
        <Card title={t('config.title')} sub={t('config.path')}>
            <textarea
                value={editor}
                onInput={(e) => setEditor(e.currentTarget.value)}
                spellcheck={false}
                rows={20}
                class="w-full p-2 bg-black border border-slate-700 rounded text-xs font-mono text-slate-100 resize-y focus:outline-none focus:border-indigo-500"
            />
            <div class="flex flex-wrap gap-2 mt-2">
                <Button id="btn-load-config" onClick={load}>
                    {t('config.load')}
                </Button>
                <Button id="btn-save-config" primary onClick={save}>
                    {t('config.save')}
                </Button>
            </div>
            <div class="text-slate-400 text-xs mt-1">{status}</div>
        </Card>
    );
};

const Log = ({ refreshKey }: { refreshKey: number }) => {
    const [text, setText] = useState('');
    useEffect(() => {
        let cancelled = false;
        const tick = async (): Promise<void> => {
            try {
                const out = await ksuExec(`${MOD}/wg-user log 50 2>&1`);
                if (!cancelled) setText(out || t('log.empty'));
            } catch (e) {
                if (!cancelled) setText(t('log.error', { msg: errorText(e) }));
            }
        };
        void tick();
        const id = setInterval(() => void tick(), 5000);
        return () => {
            cancelled = true;
            clearInterval(id);
        };
    }, [refreshKey]);

    return (
        <Card title={t('log.title')} sub={t('log.subtitle')}>
            <pre class="bg-black border border-slate-700 rounded p-2 font-mono text-[11px] leading-relaxed max-h-80 overflow-auto whitespace-pre-wrap break-all">
                {text}
            </pre>
            <div class="flex flex-wrap gap-2 mt-2">
                <Button
                    id="btn-refresh-log"
                    onClick={() => {
                        ksuExec(`${MOD}/wg-user log 50 2>&1`)
                            .then(setText)
                            .catch((e) => setText(t('log.error', { msg: errorText(e) })));
                    }}
                >
                    {t('log.refresh')}
                </Button>
                <Button id="btn-clear-log" onClick={() => setText('')}>
                    {t('log.clear')}
                </Button>
            </div>
        </Card>
    );
};

export const App = () => {
    const [tick, setTick] = useState(0);
    useLang();
    return (
        <div>
            <Header />
            <Status refreshKey={tick} />
            <Controls onChange={() => setTick(tick + 1)} />
            <Config onChange={() => setTick(tick + 1)} />
            <Log refreshKey={tick} />
        </div>
    );
};
