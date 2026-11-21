package ui

// Частичный учёт трафика мастер-ключа: недоступный сервер больше не замораживает квоту.
// Харнесс как в sub_limits_test.go: настоящая ui.db, awg-ctrl серверов — фейк через
// Sub.ctrlFor; «в сети» — запись в a.nodes.conns, чтобы IsOnline не ходил в WSS.

import (
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// offlineServer — сервер отвалился совсем: IsOnline false, как у потерявшей связь ноды.
func offlineServer(a *App, id int) {
	a.nodes.mu.Lock()
	delete(a.nodes.conns, int64(id))
	a.nodes.mu.Unlock()
}

func onlineServer(a *App, id int) {
	a.nodes.mu.Lock()
	a.nodes.conns[int64(id)] = &nodeConn{}
	a.nodes.mu.Unlock()
}

// unreachableIDs — ids из поля payload (в объекте до HTTP это int, после JSON — float64).
func unreachableIDs(v any) []int64 {
	var out []int64
	for _, x := range arr(v) {
		switch n := x.(type) {
		case int:
			out = append(out, int64(n))
		case int64:
			out = append(out, n)
		case float64:
			out = append(out, int64(n))
		}
	}
	return out
}

// Недоступный сервер учитывается последним известным значением, а трафик доступных продолжает
// считаться: расход = доступные + последнее известное, а не замороженный ноль.
func TestQuotaPartialAggregation(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100000 WHERE id = 1")
	fake.setPeers(0, peer("WG", 100, 20))
	fake.setPeers(1, peer("WG", 200, 40))
	s.enforceMasters()
	if got := s.masterRowByID(a.db, 1).usedTotal(); got != 360 {
		t.Fatalf("полный замер: %d, ожидали 360", got)
	}

	// Сервер 1 отвалился, на сервере 0 трафик вырос.
	offlineServer(a, 1)
	fake.setPeers(0, peer("WG", 150, 30))
	dropPeersCache(s, 0)
	s.enforceMasters()
	m := s.masterRowByID(a.db, 1)
	// 150+30 доступного + 200+40 последнего известного = 420.
	if m.usedTotal() != 420 {
		t.Fatalf("неполный замер: %d, ожидали 420 (180 + 240)", m.usedTotal())
	}
	if m.BlockedReason.Valid {
		t.Fatalf("заблокирован %q при расходе ниже лимита", m.BlockedReason.String)
	}
	out := s.masterOut(m)
	if field(out, "partial") != true {
		t.Fatalf("partial не выставлен: %v", out)
	}
	if ids := unreachableIDs(field(out, "unreachable")); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("unreachable = %v, ожидали [1]", ids)
	}

	// Сервер вернулся: учитываются его настоящие счётчики, замер снова полный.
	onlineServer(a, 1)
	fake.setPeers(0, peer("WG", 150, 30))
	fake.setPeers(1, peer("WG", 210, 45))
	dropPeersCache(s, 0)
	dropPeersCache(s, 1)
	s.enforceMasters()
	m = s.masterRowByID(a.db, 1)
	if m.usedTotal() != 435 { // 180 + 255
		t.Fatalf("после возврата сервера: %d, ожидали 435", m.usedTotal())
	}
	out = s.masterOut(m)
	if field(out, "partial") != false {
		t.Fatalf("замер полный, а partial: %v", out)
	}
	if ids := unreachableIDs(field(out, "unreachable")); len(ids) != 0 {
		t.Fatalf("unreachable после возврата: %v", ids)
	}
}

// Известный итог уже достиг лимита — блокировка по трафику ставится и при недоступном сервере:
// обрыв не делает расход меньше.
func TestQuotaBlocksOnKnownTotalWithServerDown(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 500 WHERE id = 1")
	fake.setPeers(0, peer("WG", 150, 50))
	fake.setPeers(1, peer("WG", 150, 50))
	s.enforceMasters()
	if m := s.masterRowByID(a.db, 1); m.BlockedReason.Valid || m.usedTotal() != 400 {
		t.Fatalf("полный замер ниже лимита: %d / %v", m.usedTotal(), m.BlockedReason)
	}

	offlineServer(a, 1)
	fake.setPeers(0, peer("WG", 300, 150))
	dropPeersCache(s, 0)
	s.enforceMasters()
	m := s.masterRowByID(a.db, 1)
	// 450 доступного + 200 последнего известного = 650 ≥ 500.
	if m.usedTotal() != 650 {
		t.Fatalf("известный расход %d, ожидали 650", m.usedTotal())
	}
	if m.BlockedReason.String != blockTraffic {
		t.Fatalf("не заблокирован по известному итогу: %v (%q)", m.BlockedReason, m.BlockedReason.String)
	}
	out := s.masterOut(m)
	if field(out, "blocked") != true || field(out, "blockReason") != blockTraffic || field(out, "partial") != true {
		t.Fatalf("masterOut: %v", out)
	}
}

// Срок вышел — блокировка обязательна независимо от того, ответили ли серверы.
func TestQuotaExpiryWithServerDown(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100000 WHERE id = 1")
	fake.setPeers(0, peer("WG", 10, 5))
	offlineServer(a, 1)

	s.enforceMasters()
	m := s.masterRowByID(a.db, 1)
	if m.BlockedReason.Valid {
		t.Fatalf("заблокирован без причины: %q", m.BlockedReason.String)
	}
	execLimits(t, a, "UPDATE master_keys SET expires_at = "+strconv.FormatInt(now()-10, 10)+" WHERE id = 1")
	s.enforceMasters()
	m = s.masterRowByID(a.db, 1)
	if m.BlockedReason.String != blockExpired {
		t.Fatalf("просроченный ключ: %v (%q)", m.BlockedReason, m.BlockedReason.String)
	}
	out := s.masterOut(m)
	if field(out, "blocked") != true || field(out, "expired") != true || field(out, "partial") != true {
		t.Fatalf("masterOut просроченного при обрыве: %v", out)
	}
}

// Неполнота видна в самом списке ключей (GET /ui/masterkeys), а не только в ответе для
// устройств: панель берёт оттуда unreachable и рисует «нет связи».
func TestMasterListCarriesUnreachable(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	fake.setPeers(0, peer("WG", 10, 5))
	fake.setDown(1) // в сети, но awg-ctrl не ответил
	s.enforceMasters()

	app := newRouter()
	app.Mount("/ui/masterkeys", s.masterKeysRouter())
	get := func(target string) jsjson.Object {
		t.Helper()
		r := httptest.NewRequest("GET", target, nil)
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		v, err := jsjson.Decode(w.Body.Bytes())
		if err != nil {
			t.Fatalf("%s: не JSON (%d): %s", target, w.Code, w.Body)
		}
		o, _ := v.(jsjson.Object)
		return o
	}

	doc := get("/ui/masterkeys")
	keys := arr(field(doc, "keys"))
	if len(keys) != 1 {
		t.Fatalf("ключей %d: %v", len(keys), doc)
	}
	k, _ := keys[0].(jsjson.Object)
	if field(k, "partial") != true {
		t.Fatalf("список не отметил неполноту: %v", k)
	}
	if ids := unreachableIDs(field(k, "unreachable")); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("unreachable в списке = %v, ожидали [1]", ids)
	}

	// Сервер офлайн (не просто не ответил) — тоже в unreachable, иначе панель не покажет «нет связи».
	offlineServer(a, 1)
	devDoc := get("/ui/masterkeys/1/devices")
	if ids := unreachableIDs(field(devDoc, "unreachable")); len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("unreachable устройств = %v, ожидали [1]", ids)
	}

	// Замеров не было — partial не выдумываем: счётчик просто ещё не измерен.
	fresh := &Sub{a: a}
	if out := fresh.masterOut(s.masterByID(a.db, 1)); field(out, "partial") != false {
		t.Fatalf("partial без замера: %v", out)
	}
}

// Счётчик и флаг неполноты меняются, а rev конфига — нет: клиент не должен пересоздавать
// туннель из-за статистики. В /sub/v1/config новых полей не появляется.
func TestConfigRevStableWhenUsageAndPartialChange(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 10000 WHERE id = 1")
	fake.setPeers(0, peer("WG", 100, 50))
	fake.setPeers(1, peer("WG", 100, 50))
	s.enforceMasters()
	dev := s.deviceByID(a.db, 5)
	before := s.buildConfig(dev, nil)
	if field(before, "used_bytes") != int64(300) {
		t.Fatalf("до обрыва used_bytes = %v", field(before, "used_bytes"))
	}

	offlineServer(a, 1)
	fake.setPeers(0, peer("WG", 300, 100))
	dropPeersCache(s, 0)
	s.enforceMasters()
	after := s.buildConfig(s.deviceByID(a.db, 5), nil)
	if after.Get2("rev") != before.Get2("rev") {
		t.Fatalf("rev изменился от счётчика/неполноты:\n до: %s\n после: %s",
			jsjson.MustMarshal(before), jsjson.MustMarshal(after))
	}
	if field(after, "used_bytes") != int64(550) {
		t.Fatalf("used_bytes после обрыва = %v, ожидали 550", field(after, "used_bytes"))
	}
	if _, has := after.Get("partial"); has {
		t.Fatalf("в config появилось поле partial: %s", jsjson.MustMarshal(after))
	}
	if _, has := after.Get("unreachable"); has {
		t.Fatalf("в config появилось поле unreachable: %s", jsjson.MustMarshal(after))
	}
}

// Расход никогда не уменьшается: ни когда счётчики awg-ctrl обнулились (пир пересоздан), ни
// когда сервер отвалился, а его последнее известное значение меньше сохранённого итога.
func TestQuotaUsageNeverGoesBack(t *testing.T) {
	a, s, fake := newLimitsTest(t, 2)
	fixClock(t, 1700000000)
	chain(t, a, 2, "WG")
	execLimits(t, a, "UPDATE master_keys SET traffic_limit_bytes = 100000 WHERE id = 1")
	fake.setPeers(0, peer("WG", 300, 100))
	fake.setPeers(1, peer("WG", 200, 50))
	s.enforceMasters()
	if got := s.masterRowByID(a.db, 1).usedTotal(); got != 650 {
		t.Fatalf("начальный расход %d, ожидали 650", got)
	}

	// Пир на сервере 0 пересоздан: его счётчики обнулились, сумма стала меньше.
	fake.setPeers(0, peer("WG", 100, 20))
	dropPeersCache(s, 0)
	s.enforceMasters()
	if got := s.masterRowByID(a.db, 1).usedTotal(); got != 650 {
		t.Fatalf("расход уменьшился по полным данным: %d", got)
	}

	// Сервер 1 отвалился, и известное стало ещё меньше сохранённого итога — тоже не убавляем.
	offlineServer(a, 1)
	dropPeersCache(s, 0)
	s.enforceMasters()
	m := s.masterRowByID(a.db, 1)
	if m.usedTotal() != 650 {
		t.Fatalf("расход уменьшился при обрыве: %d", m.usedTotal())
	}
	if out := s.masterOut(m); field(out, "partial") != true || field(out, "usedBytes") != int64(650) {
		t.Fatalf("masterOut: %v", out)
	}
}
