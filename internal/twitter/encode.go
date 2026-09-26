package twitter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// EncodeJSON renders v as Twitter did: no HTML escaping of <, > and & by
// the encoder (text is entity-escaped beforehand, and source carries raw
// anchor markup), no trailing newline.
func EncodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil // the buffer is short-lived; cached copies are compacted
}

var callbackRE = regexp.MustCompile(`^[A-Za-z_$][0-9A-Za-z_$]*(\.[A-Za-z_$][0-9A-Za-z_$]*|\[[0-9]+\])*$`)

// ValidCallback reports whether name is a safe JSONP callback identifier.
func ValidCallback(name string) bool {
	return len(name) <= 128 && callbackRE.MatchString(name)
}

// WrapJSONP wraps a JSON body in a validated callback. The leading comment
// defuses content-sniffing attacks on the response.
func WrapJSONP(callback string, body []byte) []byte {
	out := make([]byte, 0, len(body)+len(callback)+8)
	out = append(out, "/**/"...)
	out = append(out, callback...)
	out = append(out, '(')
	out = append(out, body...)
	out = append(out, ");"...)
	return out
}

// XMLMarshaler is implemented by types with a hand-written XML form.
type XMLMarshaler interface {
	WriteXML(w *XMLWriter, name string)
}

// XMLWriter writes compact XML.
type XMLWriter struct {
	bytes.Buffer
}

// Header writes the XML declaration.
func (w *XMLWriter) Header() {
	w.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
}

// Open writes <name attrs>.
func (w *XMLWriter) Open(name string, attrs ...string) {
	w.WriteByte('<')
	w.WriteString(name)
	for i := 0; i+1 < len(attrs); i += 2 {
		w.WriteByte(' ')
		w.WriteString(attrs[i])
		w.WriteString(`="`)
		w.Attr(attrs[i+1])
		w.WriteByte('"')
	}
	w.WriteByte('>')
}

// Close writes </name>.
func (w *XMLWriter) Close(name string) {
	w.WriteString("</")
	w.WriteString(name)
	w.WriteByte('>')
}

// Elem writes <name>text</name>.
func (w *XMLWriter) Elem(name, text string, attrs ...string) {
	w.Open(name, attrs...)
	w.Text(text)
	w.Close(name)
}

// Text writes escaped character data, dropping characters XML 1.0 forbids
// (control characters in posts would otherwise make the whole document
// unparseable on the device). Quotes are left alone in element content, as
// in Twitter's own XML.
func (w *XMLWriter) Text(s string) { w.escape(s, false) }

// Attr writes an escaped attribute value.
func (w *XMLWriter) Attr(s string) { w.escape(s, true) }

func (w *XMLWriter) escape(s string, attr bool) {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		switch {
		case r == '<':
			w.WriteString("&lt;")
		case r == '>':
			w.WriteString("&gt;")
		case r == '&':
			w.WriteString("&amp;")
		case r == '"' && attr:
			w.WriteString("&quot;")
		case r == utf8.RuneError && size == 1:
			w.WriteRune('�')
		case isXMLChar(r):
			w.WriteString(s[:size])
		}
		s = s[size:]
	}
}

func isXMLChar(r rune) bool {
	return r == 0x09 || r == 0x0A || r == 0x0D ||
		(r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

// EncodeXML renders v as a complete document with the given root element.
func EncodeXML(root string, v any) []byte {
	var w XMLWriter
	w.Header()
	w.Value(root, reflect.ValueOf(v), "")
	return w.Bytes()
}

// EncodeXMLArray renders a slice as <root type="array"><item>...</item></root>.
func EncodeXMLArray(root, item string, v any) []byte {
	var w XMLWriter
	w.Header()
	w.array(root, item, true, reflect.ValueOf(v))
	return w.Bytes()
}

var (
	timeType       = reflect.TypeOf(Time{})
	searchTimeType = reflect.TypeOf(SearchTime{})
	xmlMarshaler   = reflect.TypeOf((*XMLMarshaler)(nil)).Elem()
)

// Value writes one named value.
func (w *XMLWriter) Value(name string, v reflect.Value, _ string) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			w.WriteByte('<')
			w.WriteString(name)
			w.WriteString("/>")
			return
		}
		v = v.Elem()
	}
	if v.Type().Implements(xmlMarshaler) {
		v.Interface().(XMLMarshaler).WriteXML(w, name)
		return
	}
	if reflect.PointerTo(v.Type()).Implements(xmlMarshaler) && v.CanAddr() {
		v.Addr().Interface().(XMLMarshaler).WriteXML(w, name)
		return
	}
	switch v.Type() {
	case timeType:
		w.Elem(name, v.Interface().(Time).String())
		return
	case searchTimeType:
		w.Elem(name, v.Interface().(SearchTime).String())
		return
	}
	switch v.Kind() {
	case reflect.String:
		w.Elem(name, v.String())
	case reflect.Bool:
		w.Elem(name, strconv.FormatBool(v.Bool()))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		w.Elem(name, strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		w.Elem(name, strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		w.Elem(name, strconv.FormatFloat(v.Float(), 'f', -1, 64))
	case reflect.Struct:
		w.Open(name)
		for _, f := range fieldsOf(v.Type()) {
			fv := v.Field(f.index)
			if f.omitEmpty && isNil(fv) {
				continue
			}
			if f.item != "" {
				w.array(f.name, f.item, f.array, fv)
				continue
			}
			w.Value(f.name, fv, "")
		}
		w.Close(name)
	case reflect.Slice:
		w.array(name, "item", false, v)
	case reflect.Map:
		w.Open(name)
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface()) })
		for _, k := range keys {
			w.Value(xmlName(fmt.Sprint(k.Interface())), v.MapIndex(k), "")
		}
		w.Close(name)
	default:
		w.Elem(name, fmt.Sprint(v.Interface()))
	}
}

func (w *XMLWriter) array(name, item string, typed bool, v reflect.Value) {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			break
		}
		v = v.Elem()
	}
	if typed {
		w.Open(name, "type", "array")
	} else {
		w.Open(name)
	}
	if v.IsValid() && (v.Kind() == reflect.Slice || v.Kind() == reflect.Array) {
		for i := 0; i < v.Len(); i++ {
			w.Value(item, v.Index(i), "")
		}
	}
	w.Close(name)
}

func isNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		return v.IsNil()
	}
	return false
}

type fieldInfo struct {
	index     int
	name      string
	omitEmpty bool
	item      string
	array     bool
}

var fieldCache sync.Map // reflect.Type -> []fieldInfo

func fieldsOf(t reflect.Type) []fieldInfo {
	if v, ok := fieldCache.Load(t); ok {
		return v.([]fieldInfo)
	}
	var out []fieldInfo
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jname, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		xtag := f.Tag.Get("xml")
		if xtag == "-" || jname == "-" {
			continue
		}
		xname, xopts, _ := strings.Cut(xtag, ",")
		fi := fieldInfo{index: i, name: jname}
		if xname != "" {
			fi.name = xname
		}
		if fi.name == "" {
			fi.name = f.Name
		}
		for _, o := range strings.Split(xopts, ",") {
			switch {
			case o == "omitempty":
				fi.omitEmpty = true
			case o == "array":
				fi.array = true
			case strings.HasPrefix(o, "item="):
				fi.item = strings.TrimPrefix(o, "item=")
			}
		}
		out = append(out, fi)
	}
	fieldCache.Store(t, out)
	return out
}

// WriteXML renders the rate limit hash in Twitter's Rails-style form.
func (r RateLimitStatus) WriteXML(w *XMLWriter, name string) {
	w.Open("hash")
	w.Elem("hourly-limit", strconv.Itoa(r.HourlyLimit), "type", "integer")
	w.Elem("reset-time-in-seconds", strconv.FormatInt(r.ResetTimeInSeconds, 10), "type", "integer")
	w.Elem("reset-time", timeISO(r.ResetTime), "type", "datetime")
	w.Elem("remaining-hits", strconv.Itoa(r.RemainingHits), "type", "integer")
	w.Close("hash")
}

func timeISO(t Time) string {
	return timeValue(t).UTC().Format("2006-01-02T15:04:05+00:00")
}

var badNameChars = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// xmlName makes an arbitrary map key safe to use as an element name.
func xmlName(s string) string {
	s = badNameChars.ReplaceAllString(s, "_")
	if s == "" || (s[0] >= '0' && s[0] <= '9') || s[0] == '-' || s[0] == '.' {
		s = "_" + s
	}
	return s
}
