package ctrl

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maeneko/forgetting/internal/iauth"
	"github.com/maeneko/forgetting/internal/jsjson"
	"github.com/maeneko/forgetting/internal/logx"
)

// usageTestServer — живая users.db, настоящий awg1.conf во временном каталоге и ключ
// сервера: rebuildConf/syncPeers отрабатывают до конца (сам `awg syncconf` на машине без AWG
// просто не найдётся — это предупреждение в лог, не ошибка теста).
func usageTestServer(t *testing.T) *Server {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	dir := t.TempDir()
	priv := filepath.Join(dir, "server_private.key")
	if err := os.WriteFile(priv, []byte("test-private-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "awg1.conf"), []byte("[Interface]\nPrivateKey = test-private-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, iface: "awg1", confDir: dir, serverPrivKeyFile: priv, log: logx.New(os.Stderr, t.TempDir())}
	s.shaping = newShaping(s)
	s.torrent = newTorrent(s)
	s.usage = newUsage(s)
	return s
}

func addTestUser(t *testing.T, s *Server, name, ip, pub string) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO users (name, ip, pub_key, vpn_key, psk_key) VALUES (?, ?, ?, 'vpn://x', 'psk')",
		name, ip, pub); err != nil {
		t.Fatal(err)
	}
}

func usedBytes(t *testing.T, s *Server, name string) (int64, int64) {
	t.Helper()
	var rx, tx int64
	if err := s.db.QueryRow("SELECT used_rx, used_tx FROM users WHERE name = ?", name).Scan(&rx, &tx); err != nil {
		t.Fatal(err)
	}
	return rx, tx
}

// fakePeers — управляемые показания интерфейса вместо `awg show dump`.
func fakePeers(s *Server) (map[string]peerStat, *bool) {
	stats, up := map[string]peerStat{}, true
	s.usage.peersFn = func() (map[string]peerStat, bool) {
		cp := make(map[string]peerStat, len(stats))
		for k, v := range stats {
			cp[k] = v
		}
		return cp, up
	}
	return stats, &up
}

func confHasPeer(t *testing.T, s *Server, pub string) bool {
	t.Helper()
	raw, err := os.ReadFile(s.confPath())
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(raw), "PublicKey = "+pub)
}

func TestUsageAccounting(t *testing.T) {
	s := usageTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")
	addTestUser(t, s, "petr", "10.9.0.3", "PK2")
	stats, up := fakePeers(s)
	peer := func(pub string, rx, tx float64) { stats[pub] = peerStat{Rx: rx, Tx: tx} }

	peer("PK1", 0, 0)
	s.usage.tick(1000) // новый пир: только запоминаем
	if rx, tx := usedBytes(t, s, "ivan"); rx != 0 || tx != 0 {
		t.Fatalf("первый тик не должен ничего прибавлять: %d/%d", rx, tx)
	}

	peer("PK1", 100, 50)
	s.usage.tick(1030)
	peer("PK1", 300, 120)
	s.usage.tick(1060)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 300 || tx != 120 {
		t.Fatalf("накоплено %d/%d, ждём 300/120", rx, tx)
	}

	// Счётчики обнулились (перезапуск интерфейса): прибавки нет, показания запоминаются заново.
	peer("PK1", 5, 5)
	s.usage.tick(1090)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 300 || tx != 120 {
		t.Fatalf("сброс счётчиков прибавил расход: %d/%d", rx, tx)
	}
	peer("PK1", 25, 15)
	s.usage.tick(1120)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 320 || tx != 130 {
		t.Fatalf("после сброса накоплено %d/%d, ждём 320/130", rx, tx)
	}

	// Новый пир — тоже только запоминание.
	peer("PK2", 500, 500)
	s.usage.tick(1150)
	if rx, tx := usedBytes(t, s, "petr"); rx != 0 || tx != 0 {
		t.Fatalf("новый пир: %d/%d, ждём 0/0", rx, tx)
	}
	peer("PK2", 600, 550)
	s.usage.tick(1180)
	if rx, tx := usedBytes(t, s, "petr"); rx != 100 || tx != 50 {
		t.Fatalf("петр: %d/%d, ждём 100/50", rx, tx)
	}

	// Интерфейс лёг: тик — пауза, а не сброс. Показания не меняются.
	*up = false
	peer("PK1", 999999, 999999)
	s.usage.tick(1210)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 320 || tx != 130 {
		t.Fatalf("лежащий интерфейс прибавил расход: %d/%d", rx, tx)
	}
	*up = true
	peer("PK1", 0, 0) // после подъёма счётчики с нуля — прибавки быть не может
	s.usage.tick(1240)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 320 || tx != 130 {
		t.Fatalf("сброс при подъёме интерфейса прибавил расход: %d/%d", rx, tx)
	}

	// Пир исчез с интерфейса — его показания забыты; вернулся — снова только запоминаем.
	delete(stats, "PK1")
	s.usage.tick(1270)
	peer("PK1", 5000, 5000)
	s.usage.tick(1300)
	if rx, tx := usedBytes(t, s, "ivan"); rx != 320 || tx != 130 {
		t.Fatalf("вернувшийся пир прибавил старые показания: %d/%d", rx, tx)
	}
}

func TestUsageExpiryBlocks(t *testing.T) {
	s := usageTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")
	if err := s.rebuildConf(); err != nil {
		t.Fatal(err)
	}
	if !confHasPeer(t, s, "PK1") {
		t.Fatal("до блокировки пира нет в conf")
	}
	now := time.Now().Unix()
	if _, err := s.db.Exec("UPDATE users SET expires_at = ? WHERE name = 'ivan'", now-10); err != nil {
		t.Fatal(err)
	}

	s.enforceLimits(now)

	u, err := s.userGet("ivan")
	if err != nil || u == nil {
		t.Fatalf("пользователь пропал: %v", err)
	}
	if !u.Blocked || u.BlockedAt == nil || u.BlockedReason == nil || *u.BlockedReason != "expired" {
		t.Fatalf("нет блокировки по сроку: %+v", u)
	}
	if confHasPeer(t, s, "PK1") {
		t.Fatal("заблокированный пир остался в awg1.conf")
	}
	if u.VpnKey != "vpn://x" {
		t.Fatalf("vpn_key не должен меняться: %q", u.VpnKey)
	}
}

func TestBlockTrafficLimit(t *testing.T) {
	s := usageTestServer(t)
	addTestUser(t, s, "petr", "10.9.0.3", "PK2")
	now := time.Now().Unix()
	if _, err := s.db.Exec("UPDATE users SET traffic_limit_bytes = 1000, used_rx = 600, used_tx = 399 WHERE name = 'petr'"); err != nil {
		t.Fatal(err)
	}

	s.enforceLimits(now)
	if u, _ := s.userGet("petr"); u.Blocked {
		t.Fatalf("999 из 1000 заблокировали раньше времени")
	}

	// Ровно на границе (used == limit) — блокировка.
	if _, err := s.db.Exec("UPDATE users SET used_tx = 400 WHERE name = 'petr'"); err != nil {
		t.Fatal(err)
	}
	s.enforceLimits(now)
	u, _ := s.userGet("petr")
	if !u.Blocked || u.BlockedReason == nil || *u.BlockedReason != "traffic" {
		t.Fatalf("граница лимита не заблокировала: %+v", u)
	}
	if confHasPeer(t, s, "PK2") {
		t.Fatal("пир сверх лимита остался в conf")
	}
}

func TestBlockUnblockRestores(t *testing.T) {
	s := usageTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")
	if err := s.rebuildConf(); err != nil {
		t.Fatal(err)
	}
	if !confHasPeer(t, s, "PK1") {
		t.Fatal("до блокировки пира нет в conf")
	}
	if _, err := s.db.Exec("UPDATE users SET used_rx = 777, used_tx = 223 WHERE name = 'ivan'"); err != nil {
		t.Fatal(err)
	}

	if err := s.blockUser("ivan", "manual"); err != nil {
		t.Fatal(err)
	}
	if confHasPeer(t, s, "PK1") {
		t.Fatal("после блокировки пир остался в conf")
	}

	if err := s.unblockUser("ivan"); err != nil {
		t.Fatal(err)
	}
	u, err := s.userGet("ivan")
	if err != nil || u == nil {
		t.Fatalf("пользователь пропал: %v", err)
	}
	if u.Blocked || u.BlockedAt != nil || u.BlockedReason != nil {
		t.Fatalf("разблокировка не сняла отметку: %+v", u)
	}
	if !confHasPeer(t, s, "PK1") {
		t.Fatal("разблокированный пир не вернулся в conf")
	}
	if u.UsedRx != 777 || u.UsedTx != 223 || u.UsedBytes != 1000 {
		t.Fatalf("расход потерялся при блокировке: %+v", u)
	}
}

// ───────── HTTP-контракт ─────────

func apiTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	s := usageTestServer(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s.pub = pub
	return s, iauth.Mint(priv, "test", time.Now())
}

func apiDo(t *testing.T, s *Server, tok, method, path, body string) (int, jsjson.Object) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	obj := jsjson.Object{}
	if w.Body.Len() > 0 {
		v, err := jsjson.Decode(w.Body.Bytes())
		if err != nil {
			t.Fatalf("%s %s: ответ не JSON: %s", method, path, w.Body.String())
		}
		obj, _ = v.(jsjson.Object)
	}
	return w.Code, obj
}

func findUser(t *testing.T, obj jsjson.Object, name string) jsjson.Object {
	t.Helper()
	arr, _ := obj.Get2("users").([]any)
	for _, it := range arr {
		o, _ := it.(jsjson.Object)
		if o.Get2("name") == name {
			return o
		}
	}
	t.Fatalf("пользователя %q нет в списке: %s", name, string(jsjson.MustMarshal(obj)))
	return nil
}

func TestLimitsEndpoint(t *testing.T) {
	s, tok := apiTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")

	bad := []string{
		`{"expires_at": -1}`,
		`{"expires_at": 1.5}`,
		`{"traffic_limit_bytes": "много"}`,
		`{"traffic_limit_bytes": true}`,
		`{}`,
	}
	for _, body := range bad {
		if code, resp := apiDo(t, s, tok, "PUT", "/api/users/ivan/limits", body); code != 400 {
			t.Errorf("%s: код %d, ждём 400 (%s)", body, code, string(jsjson.MustMarshal(resp)))
		}
	}

	code, resp := apiDo(t, s, tok, "PUT", "/api/users/ivan/limits", `{"expires_at": 1893456000, "traffic_limit_bytes": 1000}`)
	if code != 200 {
		t.Fatalf("код %d, ждём 200: %s", code, string(jsjson.MustMarshal(resp)))
	}
	if resp.Get2("expires_at") != float64(1893456000) || resp.Get2("traffic_limit_bytes") != float64(1000) {
		t.Fatalf("лимиты не сохранились: %s", string(jsjson.MustMarshal(resp)))
	}

	// null снимает ограничение, отсутствующее поле не трогает.
	code, resp = apiDo(t, s, tok, "PUT", "/api/users/ivan/limits", `{"expires_at": null}`)
	if code != 200 || resp.Get2("expires_at") != nil || resp.Get2("traffic_limit_bytes") != float64(1000) {
		t.Fatalf("null не снял срок: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}

	if code, _ := apiDo(t, s, tok, "PUT", "/api/users/nobody/limits", `{"expires_at": 1}`); code != 404 {
		t.Fatalf("неизвестный пользователь: код %d, ждём 404", code)
	}
}

func TestBlockEndpoints(t *testing.T) {
	s, tok := apiTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")

	if code, _ := apiDo(t, s, tok, "POST", "/api/users/ivan/block", `{"reason":"bogus"}`); code != 400 {
		t.Fatalf("неверная причина: код %d, ждём 400", code)
	}

	code, resp := apiDo(t, s, tok, "POST", "/api/users/ivan/block", `{"reason":"manual"}`)
	if code != 200 || resp.Get2("blocked") != true || resp.Get2("blocked_reason") != "manual" {
		t.Fatalf("блокировка: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}

	code, resp = apiDo(t, s, tok, "POST", "/api/users/ivan/unblock", "")
	if code != 200 || resp.Get2("blocked") != false || resp.Get2("blocked_reason") != nil {
		t.Fatalf("разблокировка: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}

	if _, err := s.db.Exec("UPDATE users SET used_rx = 10, used_tx = 20 WHERE name = 'ivan'"); err != nil {
		t.Fatal(err)
	}
	code, resp = apiDo(t, s, tok, "POST", "/api/users/ivan/usage/reset", "")
	if code != 200 || resp.Get2("used_bytes") != float64(0) || resp.Get2("used_rx") != float64(0) {
		t.Fatalf("сброс расхода: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}
}

// Блокировка по API: причина api — такая же отметка, как manual, но приходит от внешней
// системы; unblock чистит оба поля, мусорная причина — 400 и ничего не меняет.
func TestBlockAPIReason(t *testing.T) {
	s, tok := apiTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")

	blockedReason := func() (string, bool) {
		t.Helper()
		var reason sql.NullString
		var at sql.NullInt64
		if err := s.db.QueryRow("SELECT blocked_reason, blocked_at FROM users WHERE name = 'ivan'").Scan(&reason, &at); err != nil {
			t.Fatal(err)
		}
		return reason.String, reason.Valid && at.Valid
	}

	code, resp := apiDo(t, s, tok, "POST", "/api/users/ivan/block", `{"reason":"api"}`)
	if code != 200 || resp.Get2("blocked") != true || resp.Get2("blocked_reason") != "api" {
		t.Fatalf("блокировка api: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}
	if reason, ok := blockedReason(); !ok || reason != "api" {
		t.Fatalf("в БД причина %q (блокировка %v), ждём api", reason, ok)
	}

	code, resp = apiDo(t, s, tok, "POST", "/api/users/ivan/unblock", "")
	if code != 200 || resp.Get2("blocked") != false || resp.Get2("blocked_reason") != nil {
		t.Fatalf("разблокировка: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}
	if reason, ok := blockedReason(); ok || reason != "" {
		t.Fatalf("после unblock осталось %q (блокировка %v)", reason, ok)
	}

	// Мусор (в том числе причина, которой у внешнего API нет) — 400, отметки не появляется.
	for _, body := range []string{`{"reason":"bogus"}`, `{"reason":123}`, `{"reason":"API"}`} {
		if code, _ := apiDo(t, s, tok, "POST", "/api/users/ivan/block", body); code != 400 {
			t.Errorf("%s: код %d, ждём 400", body, code)
		}
	}
	if reason, ok := blockedReason(); ok {
		t.Fatalf("мусорный reason заблокировал: %q", reason)
	}
}

func TestStatsLimitsFields(t *testing.T) {
	s, tok := apiTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")
	addTestUser(t, s, "petr", "10.9.0.3", "PK2")
	now := time.Now().Unix()
	if _, err := s.db.Exec("UPDATE users SET traffic_limit_bytes = 1000, used_rx = 300, used_tx = 200, expires_at = ? WHERE name = 'ivan'", now+3600); err != nil {
		t.Fatal(err)
	}

	code, resp := apiDo(t, s, tok, "GET", "/api/users/stats", "")
	if code != 200 {
		t.Fatalf("код %d, ждём 200", code)
	}
	ivan := findUser(t, resp, "ivan")
	for k, want := range map[string]any{
		"expiresAt":         float64(now + 3600),
		"trafficLimitBytes": float64(1000),
		"usedBytes":         float64(500),
		"remainingBytes":    float64(500),
		"blocked":           false,
		"blockReason":       nil,
		"expired":           false,
	} {
		if got := ivan.Get2(k); got != want {
			t.Errorf("ivan.%s = %v, ждём %v", k, got, want)
		}
	}

	// Без лимита остаток — null, а не ноль.
	petr := findUser(t, resp, "petr")
	for _, k := range []string{"expiresAt", "trafficLimitBytes", "remainingBytes", "blockReason"} {
		if got := petr.Get2(k); got != nil {
			t.Errorf("petr.%s = %v, ждём null", k, got)
		}
	}
	if petr.Get2("usedBytes") != float64(0) {
		t.Errorf("petr.usedBytes = %v, ждём 0", petr.Get2("usedBytes"))
	}

	// Расход больше лимита — остаток прижат к нулю; истёкший срок виден сразу.
	if _, err := s.db.Exec("UPDATE users SET used_tx = 5000, expires_at = ? WHERE name = 'ivan'", now-1); err != nil {
		t.Fatal(err)
	}
	_, resp = apiDo(t, s, tok, "GET", "/api/users/stats", "")
	ivan = findUser(t, resp, "ivan")
	if ivan.Get2("remainingBytes") != float64(0) {
		t.Errorf("остаток при перерасходе: %v, ждём 0", ivan.Get2("remainingBytes"))
	}
	if ivan.Get2("expired") != true {
		t.Errorf("expired = %v, ждём true", ivan.Get2("expired"))
	}

	// Заблокированный пользователь остаётся в статистике с причиной.
	if err := s.blockUser("ivan", "expired"); err != nil {
		t.Fatal(err)
	}
	_, resp = apiDo(t, s, tok, "GET", "/api/users/stats", "")
	ivan = findUser(t, resp, "ivan")
	if ivan.Get2("blocked") != true || ivan.Get2("blockReason") != "expired" {
		t.Errorf("blocked/blockReason: %s", string(jsjson.MustMarshal(ivan)))
	}
}

// Старая users.db (без колонок лимитов) должна открыться как есть: у существующих строк
// срок/лимит NULL, расход 0, блокировки нет.
func TestUsageMigrationOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE users (
            name    TEXT PRIMARY KEY,
            ip      TEXT NOT NULL UNIQUE,
            pub_key TEXT NOT NULL,
            vpn_key TEXT NOT NULL,
            psk_key TEXT NOT NULL DEFAULT '',
            key_gen TEXT NOT NULL DEFAULT '2',
            vpn_key_prev TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO users (name, ip, pub_key, vpn_key, psk_key) VALUES ('old', '10.9.0.2', 'PKOLD', 'vpn://old', 'psk')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Server{db: db, iface: "awg1", confDir: t.TempDir(), log: logx.New(os.Stderr, t.TempDir())}
	u, err := s.userGet("old")
	if err != nil || u == nil {
		t.Fatalf("старая строка не читается: %v", err)
	}
	if u.ExpiresAt != nil || u.TrafficLimitBytes != nil || u.UsedRx != 0 || u.UsedTx != 0 || u.Blocked {
		t.Fatalf("у старой строки появились ограничения: %+v", u)
	}
}

func TestBlockedUserListedButNotInConf(t *testing.T) {
	s, tok := apiTestServer(t)
	addTestUser(t, s, "ivan", "10.9.0.2", "PK1")
	if err := s.rebuildConf(); err != nil {
		t.Fatal(err)
	}
	if err := s.blockUser("ivan", "manual"); err != nil {
		t.Fatal(err)
	}

	code, resp := apiDo(t, s, tok, "GET", "/api/users", "")
	if code != 200 {
		t.Fatalf("GET /api/users: код %d", code)
	}
	if ivan := findUser(t, resp, "ivan"); ivan.Get2("blocked") != true || ivan.Get2("vpn_key") != "vpn://x" {
		t.Fatalf("заблокированный пользователь в списке неполный: %s", string(jsjson.MustMarshal(ivan)))
	}

	code, resp = apiDo(t, s, tok, "POST", "/api/users/ivan", "")
	if code != 200 || resp.Get2("blocked") != true {
		t.Fatalf("POST /api/users/ivan: код %d, %s", code, string(jsjson.MustMarshal(resp)))
	}

	if err := s.rebuildConf(); err != nil {
		t.Fatal(err)
	}
	if confHasPeer(t, s, "PK1") {
		t.Fatal("заблокированный пир попал в пересобранный conf")
	}
}
