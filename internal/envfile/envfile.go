// Package envfile — правка cli.env (KEY=value построчно): CLI и панель пишут в него одинаково.
package envfile

import (
	"os"
	"regexp"
	"strings"
)

// Update — как String.replace без g: первое KEY=…, а нет его — строка в конец.
func Update(content, key, value string) string {
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=.*$`)
	if loc := re.FindStringIndex(content); loc != nil {
		return content[:loc[0]] + key + "=" + value + content[loc[1]:]
	}
	return strings.TrimRightFunc(content, func(r rune) bool { return r == ' ' || r == '\n' || r == '\t' || r == '\r' }) + "\n" + key + "=" + value + "\n"
}

func Remove(content, key string) string {
	var kept []string
	for _, l := range strings.Split(content, "\n") {
		if !strings.HasPrefix(l, key+"=") {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// Set — задать set и убрать remove в файле. Новый файл — 0600 (в нём пароль панели).
func Set(file string, set [][2]string, remove ...string) error {
	raw, err := os.ReadFile(file)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	content := string(raw)
	for _, kv := range set {
		content = Update(content, kv[0], kv[1])
	}
	for _, k := range remove {
		content = Remove(content, k)
	}
	return os.WriteFile(file, []byte(content), 0o600)
}
