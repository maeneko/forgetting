package ctrl

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/maeneko/forgetting/internal/jsjson"
)

func nowFn() time.Time { return time.Now() }

type ob = jsjson.Object
type kv = jsjson.KV

func ok(v any) (result, error)      { return result{200, v}, nil }
func created(v any) (result, error) { return result{201, v}, nil }
func (s *Server) routes() []route {
	a := []func(*rctx) error{s.auth}
	an := []func(*rctx) error{s.auth, validateName}
	R := func(method, path string, lock bool, mw []func(*rctx) error, h func(*rctx) (result, error)) route {
		segs := strings.Split(strings.Trim(path, "/"), "/")
		return route{method: method, segs: segs, mw: mw, lock: lock, h: h}
	}

	return []route{
		R("GET", "/health", false, nil, func(c *rctx) (result, error) {
			st := s.interfaceStatus()
			cfg := s.cfg()
			status, word := 200, "ok"
			awgSt := "ok"
			if !st.Up {
				status, word, awgSt = 503, "degraded", "down"
			}
			return result{status, ob{{"status", word}, {"server", cfg.ServerName}, {"ip", cfg.ServerIP}, {"gen", cfg.Gen},
				{"awg", ob{{"status", awgSt}, {"peers", st.Peers}, {"module", cfg.AwgModule}, {"tools", cfg.AwgTools}}}}}, nil
		}),

		R("POST", "/api/users", true, an, func(c *rctx) (result, error) {
			name, _ := c.obj().Get2("name").(string)
			if u, err := s.userGet(name); err != nil {
				return result{}, err
			} else if u != nil {
				return result{}, httpErr(409, "Пользователь уже существует")
			}
			u, err := s.addUser(name)
			if err != nil {
				return result{}, err
			}
			return created(u)
		}),

		R("GET", "/api/users", true, a, func(c *rctx) (result, error) {
			rows, err := s.db.Query("SELECT name, ip, pub_key, vpn_key, key_gen FROM users WHERE vpn_key != ''")
			if err != nil {
				return result{}, err
			}
			stats := s.getPeersData()
			list := []any{}
			for rows.Next() {
				var n, ip, pub, vk, kg string
				if err := rows.Scan(&n, &ip, &pub, &vk, &kg); err != nil {
					rows.Close()
					return result{}, err
				}
				ps := stats[pub]
				list = append(list, ob{{"name", n}, {"ip", ip}, {"pub_key", pub}, {"vpn_key", vk}, {"key_gen", kg},
					{"online", ps.Online}, {"lastHandshake", ps.LastHandshake}})
			}
			rows.Close()
			return ok(ob{{"users", list}})
		}),

		R("GET", "/api/users/stats", true, a, func(c *rctx) (result, error) {
			rows, err := s.db.Query("SELECT name, ip, pub_key, key_gen FROM users WHERE vpn_key != ''")
			if err != nil {
				return result{}, err
			}
			stats := s.getPeersData()
			list := []any{}
			for rows.Next() {
				var n, ip, pub, kg string
				if err := rows.Scan(&n, &ip, &pub, &kg); err != nil {
					rows.Close()
					return result{}, err
				}
				ps := stats[pub]
				list = append(list, ob{{"name", n}, {"ip", ip}, {"key_gen", kg}, {"online", ps.Online},
					{"lastHandshake", ps.LastHandshake}, {"rx", ps.Rx}, {"tx", ps.Tx}, {"priority", s.shaping.PriorityOf(ip)}})
			}
			rows.Close()
			return ok(ob{{"users", list}})
		}),

		// ⚠️ До «/api/users/:name», иначе тот примет «reissue» за имя пользователя.
		R("POST", "/api/users/reissue", true, a, func(c *rctx) (result, error) {
			r, err := s.reissueAll()
			if err != nil {
				return result{}, err
			}
			return ok(r)
		}),

		R("POST", "/api/users/:name", true, an, func(c *rctx) (result, error) {
			u, err := s.userGet(c.params["name"])
			if err != nil {
				return result{}, err
			}
			if u == nil {
				return result{}, httpErr(404, "Пользователь не найден")
			}
			return ok(u)
		}),

		R("DELETE", "/api/users/:name", true, an, func(c *rctx) (result, error) {
			if err := s.removeUser(c.params["name"]); err != nil {
				return result{}, err
			}
			return ok(ob{{"success", true}, {"name", c.params["name"]}})
		}),

		R("POST", "/api/peers", true, a, func(c *rctx) (result, error) {
			owner, isStr := c.obj().Get2("owner").(string)
			if !isStr || !reOwner.MatchString(owner) {
				return result{}, httpErr(400, "Неверный owner")
			}
			pub, err := checkPubKeyAny(c.obj().Get2("pub_key"))
			if err != nil {
				return result{}, err
			}
			r, err := s.addPeer(pub, owner)
			if err != nil {
				return result{}, err
			}
			return created(r)
		}),

		R("GET", "/api/peers", true, a, func(c *rctx) (result, error) {
			stats := s.getPeersData()
			peers, err := s.peersAll()
			if err != nil {
				return result{}, err
			}
			list := []any{}
			for _, p := range peers {
				ps := stats[p.PubKey]
				list = append(list, ob{{"pub_key", p.PubKey}, {"ip", p.IP}, {"psk_key", p.PskKey}, {"owner", p.Owner},
					{"online", ps.Online}, {"lastHandshake", ps.LastHandshake}, {"rx", ps.Rx}, {"tx", ps.Tx},
					{"priority", s.shaping.PriorityOf(p.IP)}})
			}
			return ok(ob{{"peers", list}})
		}),

		// Пиры, которые стоят в ядре, но неизвестны awg-ctrl (нет ни в users, ни в peers): снять
		// их после сбоя `awg set … remove` некому. Только чтение — панель сама решает, что делать.
		R("GET", "/api/audit", true, a, func(c *rctx) (result, error) {
			r := runCmd("awg", "show", s.iface, "dump")
			if !r.ok() {
				return result{}, httpErr(503, "awg show не ответил")
			}
			known, err := s.knownPubKeys()
			if err != nil {
				return result{}, err
			}
			now := float64(time.Now().Unix())
			ghosts := []any{}
			lines := strings.Split(r.Stdout, "\n")
			for _, l := range lines[1:] {
				f := strings.Split(l, "\t")
				if f[0] == "" || known[f[0]] {
					continue
				}
				num := func(i int) float64 {
					if i < len(f) {
						return orZero(jsNumber(f[i]))
					}
					return 0
				}
				hs := num(4)
				ghosts = append(ghosts, ob{{"pub_key", f[0]}, {"lastHandshake", hs}, {"online", hs > 0 && now-hs < 180}, {"rx", num(5)}, {"tx", num(6)}})
			}
			return ok(ob{{"ghosts", ghosts}})
		}),

		// Снять «призраков» из ядра. Ключи присылает панель, но здесь каждый перепроверяется:
		// пир, который успел появиться в users/peers, не трогаем. Строк в базе у «призраков»
		// нет — снимается только ядро.
		R("POST", "/api/audit/clean", true, a, func(c *rctx) (result, error) {
			arr, isArr := c.obj().Get2("pub_keys").([]any)
			if !isArr || len(arr) > syncMax {
				return result{}, httpErr(400, "Неверный список ключей")
			}
			keys, err := checkPubKeys(arr)
			if err != nil {
				return result{}, err
			}
			removed, skipped := []string{}, []string{}
			for _, k := range keys {
				known, err := s.isKnownPub(k)
				if err != nil {
					return result{}, err
				}
				skip := known
				if !skip {
					sp, err := s.getServerPublicKey()
					if err != nil {
						return result{}, err
					}
					kp := s.kernelPeers()
					skip = k == sp || kp == nil || !kp[k]
				}
				if skip {
					skipped = append(skipped, k)
				} else if s.dropFromKernel(k) {
					removed = append(removed, k)
				} else {
					skipped = append(skipped, k)
				}
			}
			if len(removed) > 0 {
				s.log.Warn("ghost peers removed from kernel", "count", len(removed), "removed", removed)
			}
			return ok(ob{{"removed", removed}, {"skipped", skipped}})
		}),

		// Должен стоять ДО PUT /api/peers/:pub — иначе тот примет «sync» за ключ.
		R("PUT", "/api/peers/sync", true, a, func(c *rctx) (result, error) {
			owner, isStr := c.obj().Get2("owner").(string)
			if !isStr || !rePanelID.MatchString(owner) {
				return result{}, httpErr(400, "Неверный owner")
			}
			arr, isArr := c.obj().Get2("keep").([]any)
			if !isArr || len(arr) > syncMax {
				return result{}, httpErr(400, "Неверный список ключей")
			}
			keep, err := checkPubKeys(arr)
			if err != nil {
				return result{}, err
			}
			r, err := s.syncOwnedPeers(owner, keep)
			if err != nil {
				return result{}, err
			}
			return ok(r)
		}),

		R("PUT", "/api/peers/:pub", true, a, func(c *rctx) (result, error) {
			oldPub, err := pubKeyFromParam(c.params["pub"])
			if err != nil {
				return result{}, err
			}
			newPub, err := checkPubKeyAny(c.obj().Get2("pub_key"))
			if err != nil {
				return result{}, err
			}
			r, err := s.replacePeer(oldPub, newPub)
			if err != nil {
				return result{}, err
			}
			return ok(r)
		}),

		R("POST", "/api/peers/:pub/psk", true, a, func(c *rctx) (result, error) {
			pub, err := pubKeyFromParam(c.params["pub"])
			if err != nil {
				return result{}, err
			}
			r, err := s.rotatePeerPSK(pub)
			if err != nil {
				return result{}, err
			}
			return ok(r)
		}),

		R("DELETE", "/api/peers/:pub", true, a, func(c *rctx) (result, error) {
			pub, err := pubKeyFromParam(c.params["pub"])
			if err != nil {
				return result{}, err
			}
			if err := s.removePeer(pub); err != nil {
				return result{}, err
			}
			return ok(ob{{"success", true}})
		}),

		// Общая часть клиентского конфига для подписки. Ключи obfuscation — именами .conf;
		// пустые (I2–I5, весь 3.x-блок на 2.0) не выводятся, как и в самом conf.
		R("GET", "/api/profile", true, a, func(c *rctx) (result, error) {
			sp, err := s.getServerPublicKey()
			if err != nil {
				return result{}, err
			}
			cfg := s.cfg()
			return ok(ob{{"name", cfg.ServerName}, {"endpoint", cfg.ServerIP + ":" + strconv.Itoa(cfg.ServerPort)},
				{"server_pub", sp}, {"gen", cfg.Gen}, {"dns", []string{cfg.DNS1, cfg.DNS2}},
				{"keepalive", cfg.Keepalive}, {"mtu", cfg.MTU}, {"awg", profileAwg(cfg.Params)}})
		}),

		R("POST", "/awg/restart", true, a, func(c *rctx) (result, error) {
			if err := s.restartAwg(); err != nil {
				return result{}, err
			}
			return ok(ob{{"success", true}})
		}),

		// Перевод сервера с 2.0 на 3.1 — включая перевыпуск всех vpn:// ключей. Ошибку отдаём
		// текстом: панель показывает её оператору, а причины отказа (старый модуль, старое ядро,
		// отказ ядра на подъёме) требуют разных действий.
		R("POST", "/awg/upgrade", true, a, func(c *rctx) (result, error) {
			if s.cfg().Gen == "3.1" {
				return result{}, httpErr(409, "Сервер уже работает на AmneziaWG 3.1")
			}
			if blocker := s.checkGen31Support(); blocker != "" {
				return result{}, httpErr(409, blocker)
			}
			r, err := s.upgradeToGen31()
			if err != nil {
				return result{}, httpErr(500, err.Error())
			}
			return ok(r)
		}),

		// Деление канала между устройствами. Путь под /api/ — иначе awg-agent на ноде его не
		// пропустит (и без PATCH: агент отвечает на него 400).
		R("GET", "/api/shaping", false, a, func(c *rctx) (result, error) { return ok(s.shaping.Status()) }),

		R("PUT", "/api/shaping", false, a, func(c *rctx) (result, error) {
			b := c.obj()
			enabled, e1 := b.Get2("enabled").(bool)
			auto, e2 := b.Get2("auto").(bool)
			if !e1 || !e2 {
				return result{}, httpErr(400, "enabled и auto должны быть true/false")
			}
			mbit := func(v any, what string) (int, error) {
				n := jsToNumber(v)
				if math.IsNaN(n) || n != math.Trunc(n) || n < 0 || n > 100_000 {
					return 0, httpErr(400, what+": целое число Мбит/с от 1 до 100000")
				}
				return int(n), nil
			}
			down, err := mbit(b.Get2("downMbit"), "Загрузка")
			if err != nil {
				return result{}, err
			}
			up, err := mbit(b.Get2("upMbit"), "Отдача")
			if err != nil {
				return result{}, err
			}
			if enabled && (down < 1 || up < 1) {
				return result{}, httpErr(400, "Для включения укажите ширину канала в обе стороны")
			}
			// tc/nft/модуль не нашлись или ядро отказало: оператору нужна причина, а не 500.
			if err := s.shaping.Update(ShapingSettings{Enabled: enabled, DownMbit: down, UpMbit: up, Auto: auto}); err != nil {
				return result{}, httpErr(500, err.Error())
			}
			return ok(s.shaping.Status())
		}),

		R("GET", "/api/metrics", false, a, func(c *rctx) (result, error) {
			d, err := s.metricsCached()
			if err != nil {
				return result{}, err
			}
			return ok(d)
		}),

		R("POST", "/api/monitor/install", false, a, func(c *rctx) (result, error) {
			if err := s.startMonitorJob("install"); err != nil {
				return result{}, err
			}
			return result{202, ob{{"running", true}, {"action", "install"}}}, nil
		}),
		R("POST", "/api/monitor/remove", false, a, func(c *rctx) (result, error) {
			if err := s.startMonitorJob("remove"); err != nil {
				return result{}, err
			}
			return result{202, ob{{"running", true}, {"action", "remove"}}}, nil
		}),
		R("GET", "/api/monitor/job", false, a, func(c *rctx) (result, error) { return ok(s.monitorView()) }),

		R("GET", "/awg/status", true, a, func(c *rctx) (result, error) { return ok(s.interfaceStatus()) }),

		R("POST", "/awg/start", true, a, func(c *rctx) (result, error) {
			st, err := s.startInterface()
			if err != nil {
				return result{}, err
			}
			return ok(st)
		}),
	}
}
