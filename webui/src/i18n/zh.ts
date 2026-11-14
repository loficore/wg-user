import type { Catalogue } from './en';

export const zh: Catalogue = {
    'app.subtitle': '用户态 WireGuard 代理',
    'app.language': '语言',

    'status.title': '状态',
    'status.running': '运行中',
    'status.stopped': '已停止',
    'status.checking': '检查中…',

    'controls.title': '控制',
    'controls.start': '启动',
    'controls.stop': '停止',
    'controls.restart': '重启',
    'controls.reload': '重载',

    'transport.label': '传输',
    'transport.connected': '已连接',
    'transport.reconnecting': '重连中',
    'transport.off': '未启用',
    'transport.unknown': '未知',

    'config.title': '配置',
    'config.path': '（etc/wireproxy.conf）',
    'config.load': '读取',
    'config.save': '保存并重载',
    'config.loaded': '已读取。',
    'config.saved': '已保存并重载。',
    'config.loadError': '读取失败：{msg}',
    'config.saveError': '保存失败：{msg}',

    'log.title': '日志',
    'log.subtitle': '（最后 50 行）',
    'log.refresh': '刷新',
    'log.clear': '清空',
    'log.empty': '（暂无日志）',
    'log.error': '错误：{msg}',

    'toast.failed': '{cmd} 失败：{msg}',
};
