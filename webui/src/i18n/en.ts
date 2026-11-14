// English message catalogue. This file is the source of truth for the key set:
// every other catalogue is typed as Record<MessageKey, string>, so a missing
// translation is a compile error.

export const en = {
    'app.subtitle': 'Userspace WireGuard Proxy',
    'app.language': 'Language',

    'status.title': 'Status',
    'status.running': 'Running',
    'status.stopped': 'Stopped',
    'status.checking': 'checking…',

    'controls.title': 'Controls',
    'controls.start': 'Start',
    'controls.stop': 'Stop',
    'controls.restart': 'Restart',
    'controls.reload': 'Reload',

    'transport.label': 'Transport',
    'transport.connected': 'Connected',
    'transport.connecting': 'Connecting…',
    'transport.reconnecting': 'Reconnecting',
    'transport.off': 'Off',
    'transport.unknown': 'Unknown',

    'config.title': 'Config',
    'config.path': '(etc/wireproxy.conf)',
    'config.load': 'Load',
    'config.save': 'Save & Reload',
    'config.loaded': 'Loaded.',
    'config.saved': 'Saved & reloaded.',
    'config.loadError': 'Load error: {msg}',
    'config.saveError': 'Save error: {msg}',

    'log.title': 'Log',
    'log.subtitle': '(last 50 lines)',
    'log.refresh': 'Refresh',
    'log.clear': 'Clear',
    'log.empty': '(no log)',
    'log.error': 'Error: {msg}',

    'toast.failed': '{cmd} failed: {msg}',
} as const;

export type MessageKey = keyof typeof en;
export type Catalogue = Record<MessageKey, string>;
