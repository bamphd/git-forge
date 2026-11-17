package main

import (
	"math"
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) *JVal {
	t.Helper()
	v, err := ParseJSON([]byte(doc))
	if err != nil {
		t.Fatalf("ParseJSON(%q): %v", doc, err)
	}
	return v
}

func TestParseScalars(t *testing.T) {
	cases := []struct {
		doc  string
		kind JKind
	}{
		{"null", KNull},
		{"true", KBool},
		{"false", KBool},
		{"0", KNum},
		{"-17", KNum},
		{"3.25", KNum},
		{"1e10", KNum},
		{"1E-3", KNum},
		{`"hello"`, KStr},
		{"[]", KArr},
		{"{}", KObj},
	}
	for _, c := range cases {
		v := mustParse(t, c.doc)
		if v.Kind != c.kind {
			t.Errorf("%s: kind %v, want %v", c.doc, v.Kind, c.kind)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"", " ", "{", "[", `{"a":}`, `{"a" 1}`, `{"a":1,}`, `[1,]`,
		"tru", "nul", "01", "-01", "1.", ".5", "1e", "1e+", "+1",
		"NaN", "Infinity", `"unterminated`, `"bad\q"`, `"bad\uzzzz"`,
		"\"ctrl\x01\"", "true false", "{} {}", "[1] x", `{"a":1} {"b":2}`,
		"{'a':1}", "0x10",
	}
	for _, doc := range bad {
		if _, err := ParseJSON([]byte(doc)); err == nil {
			t.Errorf("ParseJSON(%q) unexpectedly succeeded", doc)
		}
	}
}

func TestParseStringEscapes(t *testing.T) {
	v := mustParse(t, `"\n\t\"\\\/\b\f\r"`)
	if v.Str != "\n\t\"\\/\b\f\r" {
		t.Errorf("escapes: %q", v.Str)
	}
	v = mustParse(t, `"\u0041\u00e9"`)
	if v.Str != "Aé" {
		t.Errorf("unicode escapes: %q", v.Str)
	}
	// Surrogate pair decodes to the astral character.
	v = mustParse(t, `"\ud83d\ude00"`)
	if v.Str != "😀" {
		t.Errorf("surrogate pair: %q", v.Str)
	}
	// A lone surrogate becomes U+FFFD.
	v = mustParse(t, `"\ud800"`)
	if v.Str != "�" {
		t.Errorf("lone surrogate: %q", v.Str)
	}
	// Invalid UTF-8 in a raw segment becomes U+FFFD.
	raw := []byte{'"', 'a', 0xff, 'b', '"'}
	v2, err := ParseJSON(raw)
	if err != nil {
		t.Fatalf("raw invalid utf8: %v", err)
	}
	if v2.Str != "a�b" {
		t.Errorf("invalid utf8: %q", v2.Str)
	}
}

func TestDuplicateKeysKeepFirstPositionLastValue(t *testing.T) {
	v := mustParse(t, `{"a":1,"b":2,"a":3}`)
	if len(v.Keys) != 2 || v.Keys[0] != "a" || v.Keys[1] != "b" {
		t.Fatalf("keys: %v", v.Keys)
	}
	if got, _ := v.Get("a").AsInt(); got != 3 {
		t.Errorf("a = %v", v.Get("a"))
	}
}

func TestRoundTripPreservesOrderAndNumbers(t *testing.T) {
	doc := `{"z":1,"a":[true,null,"x"],"m":{"n":-0.5e+2}}`
	v := mustParse(t, doc)
	if got := string(MarshalJSON(v)); got != doc {
		t.Errorf("round trip:\n got %s\nwant %s", got, doc)
	}
	// A number's raw spelling survives a round trip.
	v = mustParse(t, `{"x":1.50}`)
	if got := string(MarshalJSON(v)); got != `{"x":1.50}` {
		t.Errorf("number spelling: %s", got)
	}
}

func TestStringifyEscaping(t *testing.T) {
	v := JObj().Set("s", JStr("a\"b\\c\nd\te\x01f\x1fg\x7fh\u00e9\u2028i"))
	got := string(MarshalJSON(v))
	// Build the expected form explicitly: controls below 0x20 escape,
	// DEL and non-ASCII stay literal.
	want := `{"s":"a\"b\\c\nd\te` + "\\u0001" + "f" + "\\u001f" + "g\x7fh\u00e9\u2028i\"}"
	if got != want {
		t.Errorf("escaping:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "\\u00e9") || strings.Contains(got, "\\u2028") {
		t.Errorf("non-ASCII must stay literal: %q", got)
	}
}

func TestAsInt(t *testing.T) {
	cases := []struct {
		doc  string
		want int64
		ok   bool
	}{
		{`42`, 42, true},
		{`-7`, -7, true},
		{`0`, 0, true},
		{`5.0`, 5, true},
		{`1e3`, 1000, true},
		{`2.5`, 0, false},
		{`9007199254740991`, 9007199254740991, true},
		{`9007199254740992`, 0, false},
		{`1e30`, 0, false},
	}
	for _, c := range cases {
		v := mustParse(t, c.doc)
		got, ok := v.AsInt()
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: AsInt = (%d,%v), want (%d,%v)", c.doc, got, ok, c.want, c.ok)
		}
	}
	if _, ok := mustParse(t, `"42"`).AsInt(); ok {
		t.Errorf("string must not parse as int")
	}
}

func TestParseJSNumber(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{"", 0, true},
		{"   ", 0, true},
		{"42", 42, true},
		{" 42 ", 42, true},
		{"+42", 42, true},
		{"-7", -7, true},
		{"2.5", 2.5, true},
		{".5", 0.5, true},
		{"5.", 5, true},
		{"1e3", 1000, true},
		{"1E-2", 0.01, true},
		{"0x10", 16, true},
		{"0Xff", 255, true},
		{"0b101", 5, true},
		{"0o17", 15, true},
		{"Infinity", math.Inf(1), true},
		{"-Infinity", math.Inf(-1), true},
		{"NaN", 0, false},
		{"abc", 0, false},
		{"1_000", 0, false},
		{"0x", 0, false},
		{"0b2", 0, false},
		{"12a", 0, false},
		{"--1", 0, false},
		{"1e999", math.Inf(1), true},
		{"-1e999", math.Inf(-1), true},
	}
	for _, c := range cases {
		got, ok := ParseJSNumber(c.raw)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseJSNumber(%q) = (%v,%v), want (%v,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}
