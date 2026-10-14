package ctrl

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// Схема users.db — та же, что у Node-версии: существующие базы открываются как есть.
// PRAGMA не задаём (журнал по умолчанию, без WAL); ждём блокировку 5 с, как better-sqlite3.

type userRow struct {
	Name       string `json:"name"`
	IP         string `json:"ip"`
	PubKey     string `json:"pub_key"`
	VpnKey     string `json:"vpn_key"`
	PskKey     string `json:"psk_key"`
	KeyGen     string `json:"key_gen"`
	VpnKeyPrev string `json:"vpn_key_prev"`
}

type peerRow struct {
	PubKey    string
	IP        string
	PskKey    string
	Owner     string
	CreatedAt int64
}

func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Один соединение: SQLite всё равно пишет по одному, а так нет «database is locked» между
	// горутинами и Tx не конкурирует с запросами вне транзакции.
	db.SetMaxOpenConns(1)
	if err := initSchema(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func initSchema(db *sql.DB) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS users (
            name    TEXT PRIMARY KEY,
            ip      TEXT NOT NULL UNIQUE,
            pub_key TEXT NOT NULL,
            vpn_key TEXT NOT NULL,
            psk_key TEXT NOT NULL DEFAULT ''
        )`,
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ip ON users (ip)",
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	// ALTER TABLE идемпотентным не бывает — смотрим фактический список колонок.
	cols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(users)")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	rows.Close()
	if !cols["key_gen"] {
		if _, err := db.Exec("ALTER TABLE users ADD COLUMN key_gen TEXT NOT NULL DEFAULT '2'"); err != nil {
			return err
		}
	}
	if !cols["vpn_key_prev"] {
		if _, err := db.Exec("ALTER TABLE users ADD COLUMN vpn_key_prev TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS config (
            key   TEXT PRIMARY KEY,
            value TEXT NOT NULL
        )`,
		// Пиры устройств sen://: приватного ключа здесь нет и быть не может.
		`CREATE TABLE IF NOT EXISTS peers (
            pub_key    TEXT PRIMARY KEY,
            ip         TEXT NOT NULL UNIQUE,
            psk_key    TEXT NOT NULL,
            owner      TEXT NOT NULL DEFAULT '',
            created_at INTEGER NOT NULL
        )`,
	} {
		if _, err := db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

func (s *Server) getCfg(key, fallback string) string {
	var v string
	if err := s.db.QueryRow("SELECT value FROM config WHERE key = ?", key).Scan(&v); err != nil {
		return fallback
	}
	return v
}

func (s *Server) setCfg(key, value string) {
	if _, err := s.db.Exec("INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)", key, value); err != nil {
		s.log.Error("setCfg failed", "key", key, "error", err)
	}
}

const userCols = "name, ip, pub_key, vpn_key, psk_key, key_gen, vpn_key_prev"

func scanUsers(rows *sql.Rows) ([]userRow, error) {
	defer rows.Close()
	out := []userRow{}
	for rows.Next() {
		var u userRow
		if err := rows.Scan(&u.Name, &u.IP, &u.PubKey, &u.VpnKey, &u.PskKey, &u.KeyGen, &u.VpnKeyPrev); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Server) usersAll() ([]userRow, error) {
	rows, err := s.db.Query("SELECT " + userCols + " FROM users")
	if err != nil {
		return nil, err
	}
	return scanUsers(rows)
}

func (s *Server) userGet(name string) (*userRow, error) {
	rows, err := s.db.Query("SELECT "+userCols+" FROM users WHERE name = ?", name)
	if err != nil {
		return nil, err
	}
	us, err := scanUsers(rows)
	if err != nil || len(us) == 0 {
		return nil, err
	}
	return &us[0], nil
}

func scanPeers(rows *sql.Rows) ([]peerRow, error) {
	defer rows.Close()
	out := []peerRow{}
	for rows.Next() {
		var p peerRow
		if err := rows.Scan(&p.PubKey, &p.IP, &p.PskKey, &p.Owner, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Server) peersAll() ([]peerRow, error) {
	rows, err := s.db.Query("SELECT pub_key, ip, psk_key, owner, created_at FROM peers ORDER BY created_at, rowid")
	if err != nil {
		return nil, err
	}
	return scanPeers(rows)
}

func (s *Server) peerGet(pub string) (*peerRow, error) {
	rows, err := s.db.Query("SELECT pub_key, ip, psk_key, owner, created_at FROM peers WHERE pub_key = ?", pub)
	if err != nil {
		return nil, err
	}
	ps, err := scanPeers(rows)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	return &ps[0], nil
}

func (s *Server) userPubTaken(q queryer, pub string) (bool, error) {
	var n int
	if err := q.QueryRow("SELECT COUNT(*) AS n FROM users WHERE pub_key = ?", pub).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// nextIp: первый свободный 10.9.c.d (c = 0..255, d = 2..254) среди users и peers.
func (s *Server) nextIp(q queryer, subnet string) (string, error) {
	rows, err := q.Query("SELECT ip FROM users UNION SELECT ip FROM peers")
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			rows.Close()
			return "", err
		}
		used[ip] = true
	}
	rows.Close()
	for c := 0; c <= 255; c++ {
		for d := 2; d <= 254; d++ {
			ip := fmt.Sprintf("%s.%d.%d", subnet, c, d)
			if !used[ip] {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("Подсеть заполнена")
}
