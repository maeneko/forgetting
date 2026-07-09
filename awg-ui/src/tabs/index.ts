// Автообнаружение вкладок. Каждая вкладка — самодостаточная папка tabs/<name>/:
//   index.tsx      — компонент + логика
//   <name>.css     — стили вкладки (импортит сам компонент)
//   metadata.json  — { id, label, icon, order?, chrome? } (манифест: описание + порядок)
// Реестр сам находит все папки через import.meta.glob и собирает «живые» части
// (Page-компонент, Icon по имени из ICONS) к метадате. Добавить вкладку = просто
// создать папку с index.tsx и metadata.json — этот файл трогать НЕ нужно.
//   metadata.json НЕ проверяется компилятором (это JSON), поэтому содержимое
//   валидируется здесь в рантайме — кривой/неполный манифест падает с внятной
//   ошибкой при сборке TABS, а не «тихо» в UI.
import type { ComponentType } from 'react';
import type { PageProps } from '../lib/shared';
import { ICONS } from '../components/icons';

// Видимость общего хрома (сервер-бар) для вкладки. Любой флаг по умолчанию true.
export interface TabChrome {
    serverBar?: boolean;
    addServer?: boolean;
    restart?:   boolean;
}

// Сериализуемая часть из metadata.json.
export interface TabMeta {
    id:      string;
    label:   string;
    icon:    string;     // имя иконки → ICONS
    order?:  number;     // порядок в меню (по возрастанию; без него — 0)
    chrome?: TabChrome;
}

// Собранная вкладка: метадата + резолвнутые Icon и Page.
export interface TabDef extends TabMeta {
    Icon: () => JSX.Element;
    Page: ComponentType<PageProps>;
}

const metas = import.meta.glob<{ default: unknown }>('./*/metadata.json', { eager: true });
const pages = import.meta.glob<{ default: ComponentType<PageProps> }>('./*/index.tsx', { eager: true });

// './users/metadata.json' → 'users'
const folderOf = (path: string) => path.split('/')[1];

// Валидация сырого metadata.json → типизированный TabMeta (или внятная ошибка).
function toMeta(name: string, raw: unknown): TabMeta {
    const m = (raw ?? {}) as Record<string, unknown>;
    const str = (key: string): string => {
        const v = m[key];
        if (typeof v !== 'string' || v.trim() === '') {
            throw new Error(`tabs/${name}/metadata.json: поле "${key}" должно быть непустой строкой`);
        }
        return v;
    };
    if (m.order !== undefined && typeof m.order !== 'number') {
        throw new Error(`tabs/${name}/metadata.json: "order" должно быть числом`);
    }
    if (m.chrome !== undefined && (typeof m.chrome !== 'object' || m.chrome === null)) {
        throw new Error(`tabs/${name}/metadata.json: "chrome" должно быть объектом`);
    }
    return {
        id:     str('id'),
        label:  str('label'),
        icon:   str('icon'),
        order:  m.order as number | undefined,
        chrome: m.chrome as TabChrome | undefined,
    };
}

export const TABS: TabDef[] = Object.entries(metas)
    .map(([path, mod]) => {
        const name = folderOf(path);
        const meta = toMeta(name, mod.default);
        const page = pages[`./${name}/index.tsx`]?.default;
        if (!page) {
            throw new Error(`tabs/${name}: нет index.tsx рядом с metadata.json`);
        }
        if (!ICONS[meta.icon]) {
            throw new Error(`tabs/${name}: иконка "${meta.icon}" не зарегистрирована в ICONS (components/icons.tsx)`);
        }
        return { ...meta, Icon: ICONS[meta.icon], Page: page };
    })
    .sort((a, b) => (a.order ?? 0) - (b.order ?? 0));

// id вкладок используются как ключ активной вкладки — дубли ломают переключение.
const ids = TABS.map(t => t.id);
const dup = ids.find((id, i) => ids.indexOf(id) !== i);
if (dup) {
    throw new Error(`tabs: дублирующийся id "${dup}" — id вкладок должны быть уникальны`);
}
