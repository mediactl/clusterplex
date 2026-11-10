// Package extradata writes a row's extra_data the way PMS writes it: a
// compact JSON object of the fields in key order, and beside them "url",
// the same pairs as one query string in PMS's own escaping.
package extradata

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

// Encode is fields as PMS's extra_data.
func Encode(fields map[string]string) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var q strings.Builder
	for i, k := range keys {
		if i > 0 {
			q.WriteByte('&')
		}
		q.WriteString(Escape(k) + "=" + Escape(fields[k]))
	}
	var b bytes.Buffer
	b.WriteByte('{')
	write := func(k, v string) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.Write(jsonString(k))
		b.WriteByte(':')
		b.Write(jsonString(v))
	}
	for _, k := range keys {
		write(k, fields[k])
	}
	write("url", q.String())
	b.WriteByte('}')
	return b.String()
}

// Decode reads an extra_data's fields, without its "url".
func Decode(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	delete(out, "url")
	return out, nil
}

// jsonString is s as a JSON string without HTML escaping, as PMS writes it.
func jsonString(s string) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(s)
	return bytes.TrimRight(b.Bytes(), "\n")
}

// Escape percent-encodes every byte but letters, digits, '-' and '_',
// upper-case: PMS escapes '.', ':' and '/' too ("provider-static%2Eplex%2Etv").
func Escape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '_':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
