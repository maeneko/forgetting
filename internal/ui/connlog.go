package ui

// Журнал VPN-подключений (вкладка «Подключения»): события ведёт awg-ctrl каждого сервера
// (internal/ctrl/connlog.go), панель забирает их у выбранного сервера и подставляет имена, которых
// awg-ctrl не знает: пир sen:// там — только владелец «<панель>/m<ключ>/d<устройство>».

import (
	"net/url"
	"regexp"
	"strconv"

	"github.com/maeneko/forgetting/internal/jsjson"
)

var reOwner = regexp.MustCompile(`^(?:([a-z0-9]{8,16})/)?m(\d+)/d(\d+)$`)

// ownerNames — название мастер-ключа и имя устройства для владельца пира. foreign — пир другой
// панели на том же awg-ctrl (у ноды может быть своя): её имён мы не знаем.
func (s *Sub) ownerNames(owner string, cache map[string][3]any) (key, device any, foreign bool) {
	if v, ok := cache[owner]; ok {
		return v[0], v[1], v[2].(bool)
	}
	m := reOwner.FindStringSubmatch(owner)
	switch {
	case m == nil, m[1] != "" && m[1] != s.panelID: // не наш формат или чужая панель
		foreign = true
	default:
		mid, _ := strconv.ParseInt(m[2], 10, 64)
		did, _ := strconv.ParseInt(m[3], 10, 64)
		var label, name string
		if s.a.db.QueryRow("SELECT label FROM master_keys WHERE id = ?", mid).Scan(&label) == nil {
			key = label
		}
		if s.a.db.QueryRow("SELECT device_name FROM devices WHERE id = ?", did).Scan(&name) == nil {
			device = name
		}
	}
	cache[owner] = [3]any{key, device, foreign}
	return key, device, foreign
}

func (a *App) connlog(c *Ctx) bool {
	q := url.Values{}
	for _, k := range []string{"limit", "before", "q"} {
		if v := c.R.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	r, err := a.nodes.CtrlFor(a.serverIDOf(c))("GET", "/api/connlog?"+q.Encode(), nil)
	if err != nil {
		return c.Error(502, "awg-ctrl недоступен")
	}
	if r.Status == 404 {
		return c.Error(501, "awg-ctrl на этом сервере старой версии и журнала не ведёт — обновите сервер")
	}
	data, ok := r.Data.(jsjson.Object)
	if r.Status != 200 || !ok {
		return c.JSON(r.Status, r.Data)
	}
	cache := map[string][3]any{}
	enrich := func(list any) {
		items, _ := list.([]any)
		for i, it := range items {
			o, ok := it.(jsjson.Object)
			if !ok {
				continue
			}
			if owner, _ := o.Get2("owner").(string); owner != "" && a.sub != nil {
				key, dev, foreign := a.sub.ownerNames(owner, cache)
				o = append(o, jsjson.KV{Key: "key", Val: key}, jsjson.KV{Key: "device", Val: dev}, jsjson.KV{Key: "foreign", Val: foreign})
			}
			items[i] = o
		}
	}
	enrich(data.Get2("events"))
	enrich(data.Get2("peers"))
	return c.JSON(200, data)
}
