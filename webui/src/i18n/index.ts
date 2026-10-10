import { useEffect, useState } from 'preact/hooks';
import { en, type Catalogue, type MessageKey } from './en';
import { zh } from './zh';

export type Lang = 'en' | 'zh';

export const LANGS: ReadonlyArray<{ id: Lang; label: string }> = [
    { id: 'zh', label: '中文' },
    { id: 'en', label: 'EN' },
];

const CATALOGUES: Record<Lang, Catalogue> = { en, zh };
const STORAGE_KEY = 'wg-user.lang';
const listeners = new Set<() => void>();

const detect = (): Lang => {
    try {
        const saved = localStorage.getItem(STORAGE_KEY);
        if (saved === 'en' || saved === 'zh') return saved;
    } catch {
        /* storage blocked */
    }
    const nav = (typeof navigator === 'undefined' ? 'en' : navigator.language).toLowerCase();
    return nav.startsWith('zh') ? 'zh' : 'en';
};

let current: Lang = detect();

const applyDocumentLang = (lang: Lang): void => {
    try {
        document.documentElement.lang = lang === 'zh' ? 'zh-CN' : 'en';
    } catch {
        /* ignore */
    }
};

applyDocumentLang(current);

export const getLang = (): Lang => current;

export const setLang = (lang: Lang): void => {
    if (lang === current) return;
    current = lang;
    try {
        localStorage.setItem(STORAGE_KEY, lang);
    } catch {
        /* storage blocked */
    }
    applyDocumentLang(lang);
    listeners.forEach((fn) => fn());
};

/** Force a re-render when the language changes; returns the active language. */
export const useLang = (): Lang => {
    const [, bump] = useState(0);
    useEffect(() => {
        const fn = (): void => bump((n) => n + 1);
        listeners.add(fn);
        return () => {
            listeners.delete(fn);
        };
    }, []);
    return current;
};

/** Translate `key`, substituting `{name}` placeholders from `vars`. */
export const t = (key: MessageKey, vars?: Record<string, string | number>): string => {
    let out: string = CATALOGUES[current][key] ?? en[key] ?? key;
    if (vars) {
        for (const [name, value] of Object.entries(vars)) {
            out = out.split(`{${name}}`).join(String(value));
        }
    }
    return out;
};

export type { MessageKey };
