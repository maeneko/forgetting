package ui

// ОС устройства sen:// — по свободному полю devices.platform. Нормализуем при чтении:
// в старых строках сырого значения уже нет, а клиент шлёт что угодно («Android 14»,
// «Mac OS X»), поэтому только бакеты, без самого текста: платформа — не метка для
// публичного API, а строка, которую пишет клиент (в ней может быть имя устройства).

import (
	"strings"

	"github.com/maeneko/forgetting/internal/jsjson"
)

// osOrder — фиксированный порядок платформ в ответе. other и unknown в known не входят:
// потребитель считает доли по пяти настоящим ОС, а остаток показывает отдельно.
var osOrder = []string{"android", "windows", "macos", "linux", "ios", "other", "unknown"}

// platformOS — сырое devices.platform → один из семи бакетов. Сравнение по префиксу:
// клиенты шлют «Android 14», а не «android». ios проверяется до macos (см. таблицу в
// решении): иначе «mac os x» поймал бы «mac».
func platformOS(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "unknown"
	}
	rules := []struct {
		os    string
		match []string
	}{
		{"ios", []string{"ios", "iphone", "ipad", "ipados"}},
		{"android", []string{"android", "samsung", "xiaomi", "pixel"}},
		{"windows", []string{"windows", "win"}},
		{"macos", []string{"macos", "mac os", "mac", "osx", "os x", "darwin"}},
		// Ubuntu/Debian — самые частые дистрибутивы у SenAWG-клиентов: без них Linux
		// уезжал бы в other (решение, §7: правила расширяются по реальным значениям).
		{"linux", []string{"linux", "gnu/linux", "ubuntu", "debian"}},
	}
	for _, r := range rules {
		for _, m := range r.match {
			if strings.HasPrefix(s, m) {
				return r.os
			}
		}
	}
	return "other"
}

// osRow — строка GROUP BY platform: сырое значение и сколько таких устройств.
type osRow struct {
	platform string
	n        int
}

// osCount — счётчик бакета.
type osCount struct {
	os      string
	devices int
}

// platformCounts — строки по сырым платформам → счётчики по семи бакетам в osOrder.
func platformCounts(rows []osRow) []osCount {
	counts := map[string]int{}
	for _, r := range rows {
		counts[platformOS(r.platform)] += r.n
	}
	out := make([]osCount, len(osOrder))
	for i, os := range osOrder {
		out[i] = osCount{os: os, devices: counts[os]}
	}
	return out
}

// platformBlock — блок platforms для /api/v1/monitor: total (все устройства), known
// (сумма пяти настоящих ОС, без other/unknown), by_os — всегда семь записей.
func platformBlock(rows []osRow) jsjson.Object {
	counts := platformCounts(rows)
	total, known := 0, 0
	byOS := make([]any, len(counts))
	for i, c := range counts {
		total += c.devices
		if c.os != "other" && c.os != "unknown" {
			known += c.devices
		}
		byOS[i] = jsjson.Object{{Key: "os", Val: c.os}, {Key: "devices", Val: c.devices}}
	}
	return jsjson.Object{{Key: "total", Val: total}, {Key: "known", Val: known}, {Key: "by_os", Val: byOS}}
}
