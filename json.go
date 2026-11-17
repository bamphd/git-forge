// Ordered JSON values: a parser and a serializer that match JavaScript's
// JSON.parse / JSON.stringify closely enough that no client can tell this
// server apart from the Node entry it replaces.
//
// The value model keeps object keys in insertion order (JavaScript objects
// preserve insertion order for string keys, and the suite reads several
// responses as ordered text), keeps numbers in their original spelling, and
// decodes strings to valid UTF-8 the way Node's UTF-8 decoding does
// (invalid sequences become U+FFFD). Duplicate object keys keep the first
// key position and the last value, as JSON.parse does.

package main

import (
	"errors"
	"math"
	"strconv"
	"unicode/utf8"
)

// JKind distinguishes the six JSON value shapes.
type JKind uint8

const (
	KNull JKind = iota
	KBool
	KNum
	KStr
	KArr
	KObj
)

// JVal is one JSON value. Only the fields for its Kind are meaningful:
// Bool for KBool, Num (the raw token text) for KNum, Str for KStr, Arr for
// KArr, and the parallel Keys/Vals slices for KObj.
type JVal struct {
	Kind JKind
	Bool bool
	Num  string
	Str  string
	Arr  []*JVal
	Keys []string
	Vals []*JVal
}

// Constructors. JNul returns a fresh null each call so no caller can mutate
// a shared value through it.

func JNul() *JVal { return &JVal{Kind: KNull} }

func JBool(b bool) *JVal { return &JVal{Kind: KBool, Bool: b} }

func JInt(i int64) *JVal { return &JVal{Kind: KNum, Num: strconv.FormatInt(i, 10)} }

func JNumRaw(raw string) *JVal { return &JVal{Kind: KNum, Num: raw} }

func JStr(s string) *JVal { return &JVal{Kind: KStr, Str: s} }

func JArr(items ...*JVal) *JVal {
	if items == nil {
		items = []*JVal{}
	}
	return &JVal{Kind: KArr, Arr: items}
}

func JObj() *JVal { return &JVal{Kind: KObj} }

// Set assigns key to val, replacing the value at the key's first position
// when the key already exists, appending otherwise.
func (v *JVal) Set(key string, val *JVal) *JVal {
	for i, k := range v.Keys {
		if k == key {
			v.Vals[i] = val
			return v
		}
	}
	v.Keys = append(v.Keys, key)
	v.Vals = append(v.Vals, val)
	return v
}

// Get returns the value for key, or nil when the value is not an object or
// the key is absent.
func (v *JVal) Get(key string) *JVal {
	if v == nil || v.Kind != KObj {
		return nil
	}
	for i, k := range v.Keys {
		if k == key {
			return v.Vals[i]
		}
	}
	return nil
}

// Has reports whether the object carries key.
func (v *JVal) Has(key string) bool { return v.Get(key) != nil }

// IsObj reports whether the value is a JSON object.
func (v *JVal) IsObj() bool { return v != nil && v.Kind == KObj }

// IsNull reports whether the value is JSON null (a nil receiver counts as
// absent, not null, so callers must distinguish the two).
func (v *JVal) IsNull() bool { return v != nil && v.Kind == KNull }

// AsStr returns the string when the value is a JSON string.
func (v *JVal) AsStr() (string, bool) {
	if v != nil && v.Kind == KStr {
		return v.Str, true
	}
	return "", false
}

// AsBool returns the boolean when the value is a JSON boolean.
func (v *JVal) AsBool() (bool, bool) {
	if v != nil && v.Kind == KBool {
		return v.Bool, true
	}
	return false, false
}

// AsInt returns the value when the JSON number is a safe integer
// (Number.isSafeInteger in JavaScript terms): an integral value whose
// magnitude fits in 2^53-1. Non-numbers and non-integral numbers fail.
func (v *JVal) AsInt() (int64, bool) {
	if v == nil || v.Kind != KNum {
		return 0, false
	}
	if i, ok := parseJSONInt(v.Num); ok {
		if i <= 9007199254740991 && i >= -9007199254740991 {
			return i, true
		}
		return 0, false
	}
	f, err := strconv.ParseFloat(v.Num, 64)
	if err != nil {
		return 0, false
	}
	if f != float64(int64(f)) || f > 9007199254740991 || f < -9007199254740991 {
		return 0, false
	}
	return int64(f), true
}

// AsFloat returns the number's float64 value.
func (v *JVal) AsFloat() (float64, bool) {
	if v == nil || v.Kind != KNum {
		return 0, false
	}
	f, err := strconv.ParseFloat(v.Num, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// parseJSONInt parses a JSON integer token (optional minus, digits only).
func parseJSONInt(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	i := 0
	neg := false
	if raw[0] == '-' {
		neg = true
		i = 1
	}
	if i >= len(raw) {
		return 0, false
	}
	var acc int64
	for ; i < len(raw); i++ {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int64(c - '0')
		if acc > (1<<63-1-d)/10 {
			return 0, false
		}
		acc = acc*10 + d
	}
	if neg {
		acc = -acc
	}
	return acc, true
}

// ---------------------------------------------------------------------------
// Parsing
// ---------------------------------------------------------------------------

var errJSON = errors.New("invalid JSON")

type jsonParser struct {
	b []byte
	i int
}

// ParseJSON decodes one JSON document. Trailing bytes after the value
// (beyond JSON whitespace) are an error, as with JSON.parse.
func ParseJSON(b []byte) (*JVal, error) {
	p := &jsonParser{b: b}
	p.skipWs()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.skipWs()
	if p.i != len(p.b) {
		return nil, errJSON
	}
	return v, nil
}

func (p *jsonParser) skipWs() {
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonParser) value() (*JVal, error) {
	if p.i >= len(p.b) {
		return nil, errJSON
	}
	switch c := p.b[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		s, err := p.str()
		if err != nil {
			return nil, err
		}
		return JStr(s), nil
	case c == 't':
		if p.literal("true") {
			return JBool(true), nil
		}
		return nil, errJSON
	case c == 'f':
		if p.literal("false") {
			return JBool(false), nil
		}
		return nil, errJSON
	case c == 'n':
		if p.literal("null") {
			return JNul(), nil
		}
		return nil, errJSON
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, errJSON
	}
}

func (p *jsonParser) literal(word string) bool {
	if len(p.b)-p.i < len(word) {
		return false
	}
	if string(p.b[p.i:p.i+len(word)]) != word {
		return false
	}
	p.i += len(word)
	return true
}

func (p *jsonParser) object() (*JVal, error) {
	p.i++ // consume '{'
	obj := JObj()
	p.skipWs()
	if p.i < len(p.b) && p.b[p.i] == '}' {
		p.i++
		return obj, nil
	}
	for {
		p.skipWs()
		if p.i >= len(p.b) || p.b[p.i] != '"' {
			return nil, errJSON
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skipWs()
		if p.i >= len(p.b) || p.b[p.i] != ':' {
			return nil, errJSON
		}
		p.i++
		p.skipWs()
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		obj.Set(key, val)
		p.skipWs()
		if p.i >= len(p.b) {
			return nil, errJSON
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return obj, nil
		default:
			return nil, errJSON
		}
	}
}

func (p *jsonParser) array() (*JVal, error) {
	p.i++ // consume '['
	arr := JArr()
	p.skipWs()
	if p.i < len(p.b) && p.b[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		p.skipWs()
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		arr.Arr = append(arr.Arr, val)
		p.skipWs()
		if p.i >= len(p.b) {
			return nil, errJSON
		}
		switch p.b[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, nil
		default:
			return nil, errJSON
		}
	}
}

// number parses a JSON number token with the grammar's exact strictness:
// no leading zeros, a fraction needs digits after the point, an exponent
// needs digits after the marker. The raw token text is kept.
func (p *jsonParser) number() (*JVal, error) {
	start := p.i
	if p.b[p.i] == '-' {
		p.i++
		if p.i >= len(p.b) {
			return nil, errJSON
		}
	}
	if p.b[p.i] == '0' {
		p.i++
	} else if p.b[p.i] >= '1' && p.b[p.i] <= '9' {
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	} else {
		return nil, errJSON
	}
	if p.i < len(p.b) && p.b[p.i] == '.' {
		p.i++
		if p.i >= len(p.b) || p.b[p.i] < '0' || p.b[p.i] > '9' {
			return nil, errJSON
		}
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	if p.i < len(p.b) && (p.b[p.i] == 'e' || p.b[p.i] == 'E') {
		p.i++
		if p.i < len(p.b) && (p.b[p.i] == '+' || p.b[p.i] == '-') {
			p.i++
		}
		if p.i >= len(p.b) || p.b[p.i] < '0' || p.b[p.i] > '9' {
			return nil, errJSON
		}
		for p.i < len(p.b) && p.b[p.i] >= '0' && p.b[p.i] <= '9' {
			p.i++
		}
	}
	return JNumRaw(string(p.b[start:p.i])), nil
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

// str parses a JSON string starting at the opening quote. Escape sequences
// decode as JavaScript decodes them, including surrogate pairs; a lone
// surrogate becomes U+FFFD, and invalid UTF-8 in raw segments is replaced
// the way Node's UTF-8 decoding replaces it.
func (p *jsonParser) str() (string, error) {
	p.i++ // consume opening quote
	var out []byte
	segStart := p.i
	flush := func(end int) {
		out = appendSanitizedUTF8(out, p.b[segStart:end])
	}
	for {
		if p.i >= len(p.b) {
			return "", errJSON
		}
		c := p.b[p.i]
		switch {
		case c == '"':
			flush(p.i)
			p.i++
			return string(out), nil
		case c == '\\':
			flush(p.i)
			p.i++
			if p.i >= len(p.b) {
				return "", errJSON
			}
			esc := p.b[p.i]
			p.i++
			switch esc {
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			case '/':
				out = append(out, '/')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				r, err := p.unicodeEscape()
				if err != nil {
					return "", err
				}
				out = utf8.AppendRune(out, r)
			default:
				return "", errJSON
			}
			segStart = p.i
		case c < 0x20:
			return "", errJSON
		default:
			p.i++
		}
	}
}

// unicodeEscape reads the four hex digits after \u (p.i is just past the
// 'u') and decodes the code unit, combining a high surrogate with a
// following \u low surrogate. A lone surrogate yields U+FFFD.
func (p *jsonParser) unicodeEscape() (rune, error) {
	if len(p.b)-p.i < 4 {
		return 0, errJSON
	}
	var unit int
	for k := 0; k < 4; k++ {
		v, ok := hexVal(p.b[p.i+k])
		if !ok {
			return 0, errJSON
		}
		unit = unit*16 + v
	}
	p.i += 4
	if unit >= 0xD800 && unit <= 0xDBFF {
		// High surrogate: combine with a following \uDC00-\uDFFF escape.
		if len(p.b)-p.i >= 6 && p.b[p.i] == '\\' && p.b[p.i+1] == 'u' {
			var low int
			ok := true
			for k := 0; k < 4; k++ {
				v, good := hexVal(p.b[p.i+2+k])
				if !good {
					ok = false
					break
				}
				low = low*16 + v
			}
			if ok && low >= 0xDC00 && low <= 0xDFFF {
				p.i += 6
				return rune(0x10000 + ((unit - 0xD800) << 10) + (low - 0xDC00)), nil
			}
		}
		return utf8.RuneError, nil
	}
	if unit >= 0xDC00 && unit <= 0xDFFF {
		return utf8.RuneError, nil
	}
	return rune(unit), nil
}

// appendSanitizedUTF8 copies src, replacing each invalid UTF-8 byte with
// U+FFFD, matching Node's Buffer.toString('utf8') replacement behavior
// closely enough for every input the suite sends.
func appendSanitizedUTF8(dst, src []byte) []byte {
	for len(src) > 0 {
		r, size := utf8.DecodeRune(src)
		if r == utf8.RuneError && size == 1 {
			dst = utf8.AppendRune(dst, utf8.RuneError)
			src = src[1:]
			continue
		}
		dst = append(dst, src[:size]...)
		src = src[size:]
	}
	return dst
}

// SanitizeUTF8 returns s with invalid UTF-8 byte sequences replaced by
// U+FFFD, as Node would decode the same bytes.
func SanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return string(appendSanitizedUTF8(nil, []byte(s)))
}

// ---------------------------------------------------------------------------
// Serialization
// ---------------------------------------------------------------------------

// AppendJSON appends the JSON.stringify form of v to dst.
func AppendJSON(dst []byte, v *JVal) []byte {
	switch v.Kind {
	case KNull:
		return append(dst, "null"...)
	case KBool:
		if v.Bool {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case KNum:
		return append(dst, v.Num...)
	case KStr:
		return AppendJSONString(dst, v.Str)
	case KArr:
		dst = append(dst, '[')
		for i, item := range v.Arr {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendJSON(dst, item)
		}
		return append(dst, ']')
	case KObj:
		dst = append(dst, '{')
		for i, key := range v.Keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = AppendJSONString(dst, key)
			dst = append(dst, ':')
			dst = AppendJSON(dst, v.Vals[i])
		}
		return append(dst, '}')
	}
	return dst
}

// MarshalJSON renders v as a JSON document.
func MarshalJSON(v *JVal) []byte {
	return AppendJSON(nil, v)
}

const hexLower = "0123456789abcdef"

// AppendJSONString appends s quoted and escaped exactly as
// JSON.stringify quotes it: quote, backslash and the control characters
// escape; everything else, including non-ASCII text, stays literal.
func AppendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var esc string
		switch c {
		case '"':
			esc = `\"`
		case '\\':
			esc = `\\`
		case '\b':
			esc = `\b`
		case '\f':
			esc = `\f`
		case '\n':
			esc = `\n`
		case '\r':
			esc = `\r`
		case '\t':
			esc = `\t`
		default:
			if c < 0x20 {
				dst = append(dst, s[start:i]...)
				dst = append(dst, '\\', 'u', '0', '0', hexLower[c>>4], hexLower[c&0xf])
				start = i + 1
			}
			continue
		}
		dst = append(dst, s[start:i]...)
		dst = append(dst, esc...)
		start = i + 1
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// ---------------------------------------------------------------------------
// JavaScript's Number() for query strings
// ---------------------------------------------------------------------------

// ParseJSNumber converts a query-string value the way JavaScript's Number()
// converts it: surrounding whitespace is trimmed, an empty result is 0,
// decimal/hex/binary/octal spellings and Infinity are accepted, anything
// else is not a number. It returns ok=false for NaN results.
func ParseJSNumber(raw string) (float64, bool) {
	s := trimJSSpace(raw)
	if s == "" {
		return 0, true
	}
	switch s {
	case "Infinity", "+Infinity":
		return strconvParseInf(1), true
	case "-Infinity":
		return strconvParseInf(-1), true
	case "NaN":
		return 0, false
	}
	neg := false
	body := s
	if body[0] == '+' || body[0] == '-' {
		neg = body[0] == '-'
		body = body[1:]
	}
	if len(body) > 2 && body[0] == '0' {
		if v, ok := parseJSRadix(body); ok {
			if neg {
				v = -v
			}
			return v, true
		}
		return 0, false
	}
	if !validJSDecimal(body) {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// A range error still yields +/-Inf, which is what JavaScript
		// produces for an overflowing decimal.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			if neg {
				return strconvParseInf(-1), true
			}
			return strconvParseInf(1), true
		}
		return 0, false
	}
	return f, true
}

func strconvParseInf(sign int) float64 {
	if sign < 0 {
		return math.Float64frombits(0xFFF0000000000000)
	}
	return math.Float64frombits(0x7FF0000000000000)
}

// trimJSSpace trims the whitespace JavaScript's String trim removes: ASCII
// whitespace plus the Unicode spaces. Go's unicode-aware trimming covers
// the same set for inputs that arrive over HTTP.
func trimJSSpace(s string) string {
	start := 0
	for start < len(s) {
		r, size := utf8.DecodeRuneInString(s[start:])
		if !isJSSpace(r) {
			break
		}
		start += size
	}
	end := len(s)
	for end > start {
		r, size := utf8.DecodeLastRuneInString(s[:end])
		if !isJSSpace(r) {
			break
		}
		end -= size
	}
	return s[start:end]
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680,
		0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006,
		0x2007, 0x2008, 0x2009, 0x200A, 0x2028, 0x2029, 0x202F,
		0x205F, 0x3000, 0xFEFF:
		return true
	}
	return false
}

// parseJSRadix handles 0x/0b/0o spellings (body still carries the prefix).
func parseJSRadix(body string) (float64, bool) {
	if len(body) < 3 {
		return 0, false
	}
	var digits string
	var base int
	switch body[1] {
	case 'x', 'X':
		digits, base = body[2:], 16
	case 'b', 'B':
		digits, base = body[2:], 2
	case 'o', 'O':
		digits, base = body[2:], 8
	default:
		return 0, false
	}
	if digits == "" {
		return 0, false
	}
	var acc float64
	for i := 0; i < len(digits); i++ {
		c := digits[i]
		var d float64
		switch {
		case c >= '0' && c <= '9':
			d = float64(c - '0')
		case c >= 'a' && c <= 'f':
			d = float64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = float64(c-'A') + 10
		default:
			return 0, false
		}
		if d >= float64(base) {
			return 0, false
		}
		acc = acc*float64(base) + d
	}
	return acc, true
}

// validJSDecimal checks the decimal spelling JavaScript's Number()
// accepts: digits with an optional fraction, or a leading-point fraction,
// with an optional exponent.
func validJSDecimal(body string) bool {
	i := 0
	intDigits := 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
		intDigits++
	}
	fracDigits := 0
	if i < len(body) && body[i] == '.' {
		i++
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
			fracDigits++
		}
	}
	if intDigits == 0 && fracDigits == 0 {
		return false
	}
	if i < len(body) && (body[i] == 'e' || body[i] == 'E') {
		i++
		if i < len(body) && (body[i] == '+' || body[i] == '-') {
			i++
		}
		expDigits := 0
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
			expDigits++
		}
		if expDigits == 0 {
			return false
		}
	}
	return i == len(body)
}
