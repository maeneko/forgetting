package ui

// Настройка «какие необязательные вкладки показывать»: сервер помнит выключенные
// необязательные вкладки, чтобы меню панели могло спрятать «Журнал», «Релеи»,
// «Монитор» и «Прокси», когда они не нужны. Базовые вкладки (users, masterkeys,
// settings) не выключаются никогда — они просто не входят в allowlist.
//
// Хранение — sub_meta в ui.db, как у panel_id и default_servers: без изменения схемы
// и без миграции. Ключа нет — все необязательные вкладки включены, то есть уже
// установленные панели ведут себя ровно как раньше. В базе лежит дополнение
// (выключенные), а не включённые: так «всё включено» — это и отсутствие ключа, и [].

import (
	"database/sql"
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// optionalTabs — allowlist необязательных вкладок. Порядок канонический: в нём же
// нормализуются запросы и отдаётся ответ.
var optionalTabs = []string{"connlog", "relays", "monitor", "proxy"}

// tabsMetaKey — ключ sub_meta: JSON-массив выключенных необязательных вкладок.
const tabsMetaKey = "tabs_disabled"

// tabsError — единственный текст на все неверные запросы: перечень допустимых id.
func tabsError() string { return "enabled: " + strings.Join(optionalTabs, ", ") }

// tabsAllowed — множество допустимых id необязательных вкладок.
func tabsAllowed() map[string]bool {
	m := make(map[string]bool, len(optionalTabs))
	for _, id := range optionalTabs {
		m[id] = true
	}
	return m
}

// tabsDisabled — выключенные необязательные вкладки. Испорченное или неожиданное
// значение в sub_meta не ломает панель: считаем, что выключенных нет (всё включено),
// и пишем предупреждение в журнал — как console.warn у Node-версии.
func (a *App) tabsDisabled() map[string]bool {
	out := map[string]bool{}
	var raw string
	err := a.db.QueryRow("SELECT value FROM sub_meta WHERE key = ?", tabsMetaKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return out
	}
	if err != nil {
		warnf("tabs: не прочитать %s — все вкладки включены: %v", tabsMetaKey, err)
		return out
	}
	v, err := jsjson.Decode([]byte(raw))
	if err != nil {
		warnf("tabs: испорченное значение %s (%s) — все вкладки включены: %v", tabsMetaKey, raw, err)
		return out
	}
	list, ok := v.([]any)
	if !ok {
		warnf("tabs: значение %s не массив (%s) — все вкладки включены", tabsMetaKey, raw)
		return out
	}
	allowed := tabsAllowed()
	for _, x := range list {
		// Нестроковые и неизвестные id из старой записи молча отбрасываем.
		if id, ok := x.(string); ok && allowed[id] {
			out[id] = true
		}
	}
	return out
}

// enabledTabs — включённые необязательные вкладки в каноническом порядке allowlist.
func (a *App) enabledTabs() []string {
	disabled := a.tabsDisabled()
	out := make([]string, 0, len(optionalTabs))
	for _, id := range optionalTabs {
		if !disabled[id] {
			out = append(out, id)
		}
	}
	return out
}

// tabsGet — GET /ui/tabs: включённые необязательные вкладки в порядке allowlist.
func (a *App) tabsGet(c *Ctx) bool {
	return c.JSON(200, jsjson.Object{{Key: "enabled", Val: a.enabledTabs()}})
}

// tabsPut — PUT /ui/tabs {"enabled": [...]}: нормализует список, пишет в sub_meta
// дополнение (выключенные) в одной транзакции и возвращает то, что сохранилось.
func (a *App) tabsPut(c *Ctx) bool {
	list, ok := c.Obj().Get2("enabled").([]any)
	if !ok {
		return c.Error(400, tabsError())
	}
	allowed := tabsAllowed()
	on := make(map[string]bool, len(list))
	for _, x := range list {
		id, ok := x.(string)
		if !ok || !allowed[id] {
			// Базовые вкладки и опечатки — тот же 400 с перечнем допустимых id.
			return c.Error(400, tabsError())
		}
		on[id] = true // повторный id ничего не меняет
	}
	enabled := make([]string, 0, len(optionalTabs))
	disabled := make([]string, 0, len(optionalTabs))
	for _, id := range optionalTabs {
		if on[id] {
			enabled = append(enabled, id)
		} else {
			disabled = append(disabled, id)
		}
	}
	if err := a.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO sub_meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			tabsMetaKey, string(jsjson.MustMarshal(disabled)))
		return err
	}); err != nil {
		warnf("tabs: не сохранить %s: %v", tabsMetaKey, err)
		return c.Error(500, "Не удалось сохранить настройку вкладок")
	}
	return c.JSON(200, jsjson.Object{{Key: "enabled", Val: enabled}})
}
