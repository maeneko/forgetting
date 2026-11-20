package ctrl

// Лимиты обычных vpn://-пользователей: срок действия и объём трафика.
//
// У WireGuard нет счётчика «сколько скачал пользователь» — есть только счётчики байт на пира
// в `awg show dump`, и они обнуляются при перезапуске интерфейса. Раз в 30 с запоминаем
// показания по pub_key и прибавляем к used_rx/used_tx в SQLite разницу с прошлым тиком:
//   - пира раньше не было — запоминаем показания и ничего не прибавляем (счёт начинается
//     со следующего тика);
//   - счётчик уменьшился — интерфейс перезапускали, прибавки нет, запоминаем заново;
//   - пир исчез с интерфейса — забываем его показания;
//   - интерфейс лежит — тик пропускаем целиком (это пауза, а не «все скачали ноль»).
//
// Расход пишем в users по имени, а не по pub_key: ключ перевыпускают (reissue), личность
// пользователя при этом не меняется.
//
// На том же тике — проверка лимитов: истёк срок или исчерпан трафик — пользователь
// блокируется. Блокировка не удаляет ни пользователя, ни vpn_key: строка остаётся, пир
// просто выпадает из awg1.conf и снимается с интерфейса (см. rebuildConf/syncPeers).

import (
	"math"
	"strings"
	"sync"
	"time"
)

const usageEvery = 30 * time.Second

type usageCounters struct{ rx, tx float64 }

type Usage struct {
	s       *Server
	mu      sync.Mutex
	prev    map[string]usageCounters
	peersFn func() (map[string]peerStat, bool) // подменяется в тестах
}

func newUsage(s *Server) *Usage {
	u := &Usage{s: s, prev: map[string]usageCounters{}}
	u.peersFn = func() (map[string]peerStat, bool) {
		if !s.interfaceStatus().Up {
			return nil, false // интерфейс лежит — не «тик с нулевым трафиком», а пауза
		}
		return s.getPeersData(), true
	}
	return u
}

func (u *Usage) Start() {
	go func() {
		for {
			u.tick(time.Now().Unix())
			time.Sleep(usageEvery)
		}
	}()
}

func (u *Usage) tick(now int64) {
	stats, up := u.peersFn()
	if up {
		u.accrue(stats)
	}
	u.s.enforceLimits(now)
}

// reseed забывает прошлые показания: следующий тик только запомнит их. Нужен после смены
// лимитов и сброса расхода, иначе прибавка считалась бы от старых счётчиков.
func (u *Usage) reseed() {
	u.mu.Lock()
	u.prev = map[string]usageCounters{}
	u.mu.Unlock()
}

func (s *Server) usageReseed() {
	if s.usage != nil {
		s.usage.reseed()
	}
}

// userNamesByPub — pub_key → имя пользователя. Строки читаем до конца до любых других
// запросов: соединение с SQLite одно.
func (s *Server) userNamesByPub() map[string]string {
	out := map[string]string{}
	rows, err := s.db.Query("SELECT pub_key, name FROM users")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var pub, name string
		if rows.Scan(&pub, &name) == nil {
			out[pub] = name
		}
	}
	return out
}

func (u *Usage) accrue(stats map[string]peerStat) {
	names := u.s.userNamesByPub()
	u.mu.Lock()
	defer u.mu.Unlock()
	cur := make(map[string]usageCounters, len(stats))
	adds := map[string]usageCounters{}
	for pub, st := range stats {
		if math.IsNaN(st.Rx) || math.IsNaN(st.Tx) { // строка dump не разобралась
			continue
		}
		cur[pub] = usageCounters{rx: st.Rx, tx: st.Tx}
		old, had := u.prev[pub]
		if !had {
			continue // новый пир: только запоминаем
		}
		if st.Rx < old.rx || st.Tx < old.tx {
			continue // счётчики обнулились (перезапуск интерфейса) — прибавки нет
		}
		name, known := names[pub]
		if !known {
			continue // пир sen:// или уже удалённый пользователь — расход не наш
		}
		a := adds[name]
		a.rx += max(0, st.Rx-old.rx)
		a.tx += max(0, st.Tx-old.tx)
		adds[name] = a
	}
	u.prev = cur // исчезнувшие пиры забываются вместе со своими показаниями
	for name, a := range adds {
		if a.rx == 0 && a.tx == 0 {
			continue
		}
		if _, err := u.s.db.Exec("UPDATE users SET used_rx = used_rx + ?, used_tx = used_tx + ? WHERE name = ?",
			int64(a.rx), int64(a.tx), name); err != nil {
			u.s.log.Warn("usage: не удалось записать расход", "name", name, "error", err)
		}
	}
}

// api — блокировка внешней системой через /api/v1: та же отметка, что manual, но ставится
// не оператором; expired и traffic — расчётные, их ставит тик ограничений.
var blockReasons = map[string]bool{"manual": true, "expired": true, "traffic": true, "api": true}

// enforceLimits блокирует всех, чей срок вышел или лимит трафика исчерпан. Уже
// заблокированных не трогаем: разблокировка — ручное действие, а снятая вручную блокировка
// по лимиту вернётся на следующем тике, если лимит не подняли и расход не сбросили.
func (s *Server) enforceLimits(now int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	users, err := s.usersAll()
	if err != nil {
		s.log.Error("limits: не удалось прочитать пользователей", "error", err)
		return
	}
	var expired, over []string
	for _, u := range users {
		if u.Blocked {
			continue
		}
		switch {
		case u.ExpiresAt != nil && *u.ExpiresAt <= now:
			expired = append(expired, u.Name)
		case u.TrafficLimitBytes != nil && u.UsedBytes >= *u.TrafficLimitBytes:
			over = append(over, u.Name)
		}
	}
	for _, g := range []struct {
		names  []string
		reason string
	}{{expired, "expired"}, {over, "traffic"}} {
		if len(g.names) == 0 {
			continue
		}
		if err := s.blockUsersAt(now, g.names, g.reason); err != nil {
			s.log.Error("limits: блокировка не применена", "reason", g.reason, "error", err)
		}
	}
}

// blockUsersAt ставит отметку блокировки и один раз пересобирает conf/ядро: пир уходит из
// awg1.conf, syncPeers снимает его с интерфейса. Строка users и vpn_key остаются.
func (s *Server) blockUsersAt(now int64, names []string, reason string) error {
	if !blockReasons[reason] {
		reason = "manual"
	}
	changed := 0
	for _, name := range names {
		res, err := s.db.Exec("UPDATE users SET blocked_at = ?, blocked_reason = ? WHERE name = ? AND blocked_at IS NULL",
			now, reason, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			changed++
		}
	}
	if changed == 0 {
		return nil
	}
	if err := s.syncPeers(); err != nil {
		return err
	}
	s.log.Info("users blocked", "reason", reason, "count", changed, "names", names)
	return nil
}

func (s *Server) blockUser(name, reason string) error {
	return s.blockUsersAt(time.Now().Unix(), []string{name}, reason)
}

// unblockUser снимает отметку и возвращает пира в conf/ядро — syncPeers добавляет его
// обратно из пересобранного конфига.
func (s *Server) unblockUser(name string) error {
	if _, err := s.db.Exec("UPDATE users SET blocked_at = NULL, blocked_reason = NULL WHERE name = ?", name); err != nil {
		return err
	}
	if err := s.syncPeers(); err != nil {
		return err
	}
	s.log.Info("user unblocked", "name", name)
	return nil
}

// setUserLimits меняет срок/лимит (nil — снять ограничение). Меняем только переданные поля:
// отсутствующий ключ означает «не трогать».
func (s *Server) setUserLimits(name string, expires *int64, expiresSet bool, limit *int64, limitSet bool) (*userRow, error) {
	u, err := s.userGet(name)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, httpErr(404, "Пользователь не найден")
	}
	sets, args := []string{}, []any{}
	if expiresSet {
		sets, args = append(sets, "expires_at = ?"), append(args, nullableInt(expires))
	}
	if limitSet {
		sets, args = append(sets, "traffic_limit_bytes = ?"), append(args, nullableInt(limit))
	}
	if len(sets) > 0 {
		args = append(args, name)
		if _, err := s.db.Exec("UPDATE users SET "+strings.Join(sets, ", ")+" WHERE name = ?", args...); err != nil {
			return nil, err
		}
		// Счётчики трафика остаются, но прошлые показания интерфейса к ним больше не относятся.
		s.usageReseed()
		s.log.Info("user limits changed", "name", name, "set", strings.Join(sets, ", "))
	}
	return s.userGet(name)
}

// resetUsage обнуляет расход пользователя. Причина блокировки не снимается: за этим —
// отдельный unblock.
func (s *Server) resetUsage(name string) error {
	res, err := s.db.Exec("UPDATE users SET used_rx = 0, used_tx = 0 WHERE name = ?", name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return httpErr(404, "Пользователь не найден")
	}
	s.usageReseed()
	s.log.Info("user usage reset", "name", name)
	return nil
}

func nullableInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}
