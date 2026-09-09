# Вкладки

Вкладок пока нет — каркас работает и с пустой папкой (оболочка покажет заглушку).

## Как добавить вкладку

Создай папку `src/tabs/<name>/` с тремя файлами — реестр (`tabs/index.ts`)
подхватит её сам, править его **не нужно**.

```
src/tabs/reports/
  metadata.json    { "id": "reports", "label": "Отчёты", "icon": "settings", "order": 1 }
  index.tsx        export default function ReportsPage({ token, showMsg }: PageProps) { … }
  reports.css      стили только этой вкладки (импортит сам index.tsx)
```

**metadata.json**

| поле    | тип    | обяз. | смысл                                              |
|---------|--------|-------|----------------------------------------------------|
| `id`    | string | да    | ключ активной вкладки, должен быть уникальным       |
| `label` | string | да    | подпись в меню и заголовок раздела                  |
| `icon`  | string | да    | имя из `ICONS` в `src/components/icons.tsx`         |
| `order` | number | нет   | порядок в меню по возрастанию (без него — 0)        |

Манифест валидируется в рантайме: опечатка или незарегистрированная иконка
падают с внятной ошибкой при сборке `TABS`, а не «тихо» в UI.

**index.tsx** — дефолтный экспорт компонента `ComponentType<PageProps>`,
где `PageProps = { token, showMsg }` (`src/lib/shared.ts`). Данные вкладка
грузит сама: `apiFetch('GET', '/api/…', token)`.

Новую иконку сначала добавь в `src/components/icons.tsx` и зарегистрируй
в карте `ICONS` — на её имя и ссылается `metadata.json`.
