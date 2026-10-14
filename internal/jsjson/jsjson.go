// Package jsjson повторяет JSON.stringify из JavaScript байт в байт.
//
// Зачем: выход Node-версии зашит в форматы, которые проверяют клиенты и хэши —
// распакованное содержимое vpn://, rev конфига sen:// (sha256 от строки),
// подписанные тела ответов /sub/v1, /api/profile. encoding/json отличается
// (экранирует < > & и U+2028/2029, иначе пишет числа, добавляет перевод
// строки, сортирует ключи map), поэтому здесь свой сериализатор.
//
// Порядок ключей — порядок объявления: Object хранит пары как есть, у структур
// берутся поля по порядку (тег json задаёт имя, "-" — пропуск, omitempty —
// пропуск нулевого значения, как undefined в JS).
package jsjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// KV — пара ключ/значение упорядоченного объекта.
type KV struct {
	Key string
	Val any
}

// Object — JS-объект с сохранённым порядком ключей.
type Object []KV

// Get возвращает значение по ключу.
func (o Object) Get(key string) (any, bool) {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Val, true
		}
	}
	return nil, false
}

// Raw — уже готовый JSON, вставляется как есть (без проверки).
type Raw []byte

// Marshal — аналог JSON.stringify(v).
func Marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, reflect.ValueOf(v), "", ""); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// MarshalIndent — аналог JSON.stringify(v, null, indent) (у нас indent = "  ").
func MarshalIndent(v any, indent string) ([]byte, error) {
	var b bytes.Buffer
	if err := encode(&b, reflect.ValueOf(v), indent, ""); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// MustMarshal — для значений, которые заведомо сериализуемы.
func MustMarshal(v any) []byte {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

var (
	objectType = reflect.TypeOf(Object(nil))
	rawType    = reflect.TypeOf(Raw(nil))
	jsonRaw    = reflect.TypeOf(json.RawMessage(nil))
	jsonNumber = reflect.TypeOf(json.Number(""))
)

func encode(b *bytes.Buffer, v reflect.Value, indent, cur string) error {
	if !v.IsValid() {
		b.WriteString("null")
		return nil
	}
	switch v.Type() {
	case objectType:
		return encodeObject(b, v.Interface().(Object), indent, cur)
	case rawType:
		b.Write(v.Bytes())
		return nil
	case jsonRaw:
		raw := v.Bytes()
		if len(raw) == 0 {
			b.WriteString("null")
			return nil
		}
		// JSON.stringify(JSON.parse(x)): нормализуем, не доверяя форме исходника.
		val, err := Decode(raw)
		if err != nil {
			return err
		}
		return encode(b, reflect.ValueOf(val), indent, cur)
	case jsonNumber:
		f, err := strconv.ParseFloat(v.String(), 64)
		if err != nil {
			return err
		}
		b.WriteString(Number(f))
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			b.WriteString("null")
			return nil
		}
		return encode(b, v.Elem(), indent, cur)
	case reflect.Bool:
		if v.Bool() {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case reflect.String:
		writeString(b, v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(Number(float64(v.Int())))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		b.WriteString(Number(float64(v.Uint())))
	case reflect.Float32, reflect.Float64:
		b.WriteString(Number(v.Float()))
	case reflect.Slice:
		if v.IsNil() {
			// nil-срез — это пустой массив, а не null: в JS неоткуда взяться null вместо []
			b.WriteString("[]")
			return nil
		}
		fallthrough
	case reflect.Array:
		return encodeArray(b, v, indent, cur)
	case reflect.Map:
		// В JS порядок ключей — порядок вставки; у Go-map его нет. Разрешаем только
		// map[string]..., сортируя ключи, — вызывающий отвечает за то, что это совпадает.
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("jsjson: map key must be string, got %s", v.Type().Key())
		}
		keys := v.MapKeys()
		ks := make([]string, len(keys))
		for i, k := range keys {
			ks[i] = k.String()
		}
		sortStrings(ks)
		obj := make(Object, 0, len(ks))
		for _, k := range ks {
			obj = append(obj, KV{k, v.MapIndex(reflect.ValueOf(k).Convert(v.Type().Key())).Interface()})
		}
		return encodeObject(b, obj, indent, cur)
	case reflect.Struct:
		return encodeObject(b, structObject(v), indent, cur)
	default:
		return fmt.Errorf("jsjson: unsupported kind %s", v.Kind())
	}
	return nil
}

func encodeArray(b *bytes.Buffer, v reflect.Value, indent, cur string) error {
	n := v.Len()
	if n == 0 {
		b.WriteString("[]")
		return nil
	}
	b.WriteByte('[')
	inner := cur + indent
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(inner)
		}
		// undefined/функция в массиве — null; у нас таких нет, кроме nil
		if err := encode(b, v.Index(i), indent, inner); err != nil {
			return err
		}
	}
	if indent != "" {
		b.WriteByte('\n')
		b.WriteString(cur)
	}
	b.WriteByte(']')
	return nil
}

// Omit — значение, которое JSON.stringify пропускает (undefined): ключ не выводится.
type omit struct{}

// Undefined — маркер «ключа нет», как undefined в JS-объекте.
var Undefined = omit{}

func encodeObject(b *bytes.Buffer, o Object, indent, cur string) error {
	first := true
	inner := cur + indent
	b.WriteByte('{')
	for _, kv := range o {
		if _, skip := kv.Val.(omit); skip {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(inner)
		}
		writeString(b, kv.Key)
		b.WriteByte(':')
		if indent != "" {
			b.WriteByte(' ')
		}
		if err := encode(b, reflect.ValueOf(kv.Val), indent, inner); err != nil {
			return err
		}
	}
	if !first && indent != "" {
		b.WriteByte('\n')
		b.WriteString(cur)
	}
	b.WriteByte('}')
	return nil
}

func structObject(v reflect.Value) Object {
	t := v.Type()
	obj := make(Object, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		omitEmpty := false
		if tag, ok := f.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			parts := strings.Split(tag, ",")
			if parts[0] != "" {
				name = parts[0]
			}
			for _, p := range parts[1:] {
				if p == "omitempty" {
					omitEmpty = true
				}
			}
		}
		fv := v.Field(i)
		if omitEmpty && isEmpty(fv) {
			continue
		}
		obj = append(obj, KV{name, fv.Interface()})
	}
	return obj
}

func isEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// writeString экранирует строку как JSON.stringify (ES2019): " \ и управляющие
// символы; \b \f \n \r \t коротко, прочие < 0x20 — \u00xx строчными. Не трогает
// / < > & и U+2028/2029. Неверный UTF-8 заменяется на U+FFFD (в JS его не бывает).
func writeString(b *bytes.Buffer, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if c < 0x20 {
					fmt.Fprintf(b, `\u%04x`, c)
				} else {
					b.WriteByte(c)
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteString("�")
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
}

// Number форматирует число как String(x) в JS (Number::toString), а NaN/±Infinity —
// как null, что делает JSON.stringify.
func Number(x float64) string {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return "null"
	}
	if x == 0 {
		return "0" // и -0
	}
	neg := x < 0
	if neg {
		x = -x
	}
	// Кратчайшие цифры, однозначно восстанавливающие x: d.ddddde±n
	e := strconv.FormatFloat(x, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits) // число значащих цифр
	n := exp + 1     // позиция десятичной точки (ECMA-262 Number::toString)
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		ee := n - 1
		if ee < 0 {
			ee = -ee
		}
		if k == 1 {
			out = digits + "e" + sign + strconv.Itoa(ee)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(ee)
		}
	}
	if neg {
		return "-" + out
	}
	return out
}

func sortStrings(s []string) {
	// вставками: ключей мало, а сортировка строк в JS-порядке — по UTF-16, что для
	// наших ASCII-ключей совпадает с побайтовой
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Get2 — Get без признака наличия (nil, если ключа нет); для цепочек разбора.
func (o Object) Get2(key string) any {
	v, _ := o.Get(key)
	return v
}
