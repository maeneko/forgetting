package jsjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Decode разбирает JSON с сохранением порядка ключей: объекты — Object, массивы —
// []any, числа — float64, как JSON.parse. Нужен, чтобы JSON.stringify(JSON.parse(x))
// воспроизводился точно (например, сохранённый профиль сервера с ключами awg).
func Decode(data []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := decodeValue(d)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("jsjson: trailing data")
	}
	return v, nil
}

func decodeValue(d *json.Decoder) (any, error) {
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch x := t.(type) {
	case json.Delim:
		switch x {
		case '{':
			obj := Object{}
			for d.More() {
				kt, err := d.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, errors.New("jsjson: object key is not a string")
				}
				val, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				// повторный ключ в JS перезаписывает значение, оставаясь на первом месте
				replaced := false
				for i := range obj {
					if obj[i].Key == key {
						obj[i].Val = val
						replaced = true
						break
					}
				}
				if !replaced {
					obj = append(obj, KV{key, val})
				}
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for d.More() {
				val, err := decodeValue(d)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := d.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("jsjson: unexpected delimiter")
	case json.Number:
		return x.Float64()
	default:
		return x, nil // string, bool, nil
	}
}
