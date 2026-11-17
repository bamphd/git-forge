// The GraphQL surface: a small, self-contained executor for the fixed set
// of documents the conformance suite sends (repository/issue/pull-request
// reads, introspection probes, and five mutations), plus JSON-array
// operation batching. No schema library, no code generation: a lexer, a
// recursive-descent parser for the GraphQL query language, and resolvers
// over the one store, so REST, git, GraphQL and MCP always read the same
// objects.
//
// Wire rules, as the suite asserts them: a valid document with an unknown
// field is HTTP 200 with an `errors` array naming the field; an unknown
// repository is `data.repository = null` with no errors; a batch body (a
// JSON array of operation documents) answers a JSON array of envelopes in
// order. Authentication is the transport layer's job; this module assumes
// a valid token.

package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type gqlTokenKind int

const (
	gqlName gqlTokenKind = iota
	gqlInt
	gqlFloat
	gqlString
	gqlPunct
	gqlEOF
)

type gqlToken struct {
	kind     gqlTokenKind
	value    string
	position int
}

type gqlSyntaxError struct{ message string }

func (e *gqlSyntaxError) Error() string { return e.message }

func gqlSyntax(format string, args ...any) *gqlSyntaxError {
	return &gqlSyntaxError{message: fmt.Sprintf(format, args...)}
}

func isGQLPunct(ch byte) bool {
	switch ch {
	case '!', '$', '(', ')', ':', '=', '@', '[', ']', '{', '}', '|', '&':
		return true
	}
	return false
}

func gqlLex(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	push := func(kind gqlTokenKind, value string, pos int) {
		tokens = append(tokens, gqlToken{kind: kind, value: value, position: pos})
	}
	for i < len(source) {
		ch := source[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		// A byte-order mark is ignored, like the other whitespace.
		if ch == 0xEF && i+2 < len(source) && source[i+1] == 0xBB && source[i+2] == 0xBF {
			i += 3
			continue
		}
		if ch == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
			push(gqlPunct, "...", i)
			i += 3
			continue
		}
		if isGQLPunct(ch) {
			push(gqlPunct, string(ch), i)
			i++
			continue
		}
		if ch == '"' {
			start := i
			if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
				end := strings.Index(source[i+3:], `"""`)
				if end < 0 {
					return nil, gqlSyntax("Unterminated block string")
				}
				content := source[i+3 : i+3+end]
				content = strings.ReplaceAll(content, `\"""`, `"""`)
				push(gqlString, content, start)
				i = i + 3 + end + 3
				continue
			}
			i++
			var value []byte
			for {
				if i >= len(source) {
					return nil, gqlSyntax("Unterminated string")
				}
				c := source[i]
				if c == '"' {
					break
				}
				if c == '\n' {
					return nil, gqlSyntax("Unterminated string")
				}
				if c == '\\' {
					if i+1 >= len(source) {
						return nil, gqlSyntax(`Invalid string escape \undefined`)
					}
					esc := source[i+1]
					if esc == 'u' {
						r, adv := gqlUnicodeEscape(source, i)
						value = append(value, string(r)...)
						i += adv
						continue
					}
					var rep byte
					var ok bool
					switch esc {
					case '"':
						rep, ok = '"', true
					case '\\':
						rep, ok = '\\', true
					case '/':
						rep, ok = '/', true
					case 'b':
						rep, ok = '\b', true
					case 'f':
						rep, ok = '\f', true
					case 'n':
						rep, ok = '\n', true
					case 'r':
						rep, ok = '\r', true
					case 't':
						rep, ok = '\t', true
					}
					if !ok {
						return nil, gqlSyntax(`Invalid string escape \%c`, esc)
					}
					value = append(value, rep)
					i += 2
					continue
				}
				value = append(value, c)
				i++
			}
			push(gqlString, string(value), start)
			i++
			continue
		}
		if ch >= '0' && ch <= '9' || ch == '-' {
			start := i
			text, isFloat, ok := gqlNumberToken(source[i:])
			if !ok {
				return nil, gqlSyntax("Invalid number at %d", i)
			}
			if isFloat {
				push(gqlFloat, text, start)
			} else {
				push(gqlInt, text, start)
			}
			i += len(text)
			continue
		}
		if ch == '_' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' {
			start := i
			for i < len(source) {
				c := source[i]
				if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
					i++
					continue
				}
				break
			}
			push(gqlName, source[start:i], start)
			continue
		}
		return nil, gqlSyntax("Unexpected character %s at %d", string(AppendJSONString(nil, string(ch))), i)
	}
	push(gqlEOF, "", i)
	return tokens, nil
}

// gqlUnicodeEscape decodes the \uXXXX escape at the backslash (source[i] is
// '\\', source[i+1] is 'u'), following JavaScript's lenient parseInt: a
// run of hex digits (possibly none, meaning code unit zero) supplies the
// code unit, and the escape always advances six positions. A high
// surrogate followed by a low-surrogate escape combines into one rune; a
// lone surrogate becomes U+FFFD.
func gqlUnicodeEscape(source string, i int) (rune, int) {
	end := i + 6
	if end > len(source) {
		end = len(source)
	}
	hexPart := ""
	if i+2 < len(source) {
		hexPart = source[i+2:end]
	}
	unit := 0
	digits := 0
	for k := 0; k < len(hexPart); k++ {
		v, ok := hexVal(hexPart[k])
		if !ok {
			break
		}
		unit = unit*16 + v
		digits++
	}
	if digits == 0 {
		unit = 0
	}
	advance := 6
	if unit >= 0xD800 && unit <= 0xDBFF {
		// Combine with a following \uDC00-\uDFFF escape when present.
		j := i + 6
		if j+5 < len(source) && source[j] == '\\' && source[j+1] == 'u' {
			low := 0
			ok := true
			for k := 0; k < 4; k++ {
				v, good := hexVal(source[j+2+k])
				if !good {
					ok = false
					break
				}
				low = low*16 + v
			}
			if ok && low >= 0xDC00 && low <= 0xDFFF {
				return rune(0x10000 + ((unit - 0xD800) << 10) + (low - 0xDC00)), 12
			}
		}
		return 0xFFFD, advance
	}
	if unit >= 0xDC00 && unit <= 0xDFFF {
		return 0xFFFD, advance
	}
	return rune(unit), advance
}

// gqlNumberToken matches a GraphQL number at the start of s: an optional
// minus, 0 or a nonzero digit run, an optional fraction, an optional
// exponent. It reports the matched text and whether it is a float.
func gqlNumberToken(s string) (string, bool, bool) {
	i := 0
	if i < len(s) && s[i] == '-' {
		i++
	}
	if i >= len(s) {
		return "", false, false
	}
	if s[i] == '0' {
		i++
	} else if s[i] >= '1' && s[i] <= '9' {
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	} else {
		return "", false, false
	}
	isFloat := false
	if i < len(s) && s[i] == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
		isFloat = true
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			isFloat = true
			i = j
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		}
	}
	return s[:i], isFloat, true
}

// ---------------------------------------------------------------------------
// Parser
// ---------------------------------------------------------------------------

type gqlValueKind int

const (
	gqlVar gqlValueKind = iota
	gqlIntVal
	gqlFloatVal
	gqlStringVal
	gqlBoolVal
	gqlNullVal
	gqlEnumVal
	gqlListVal
	gqlObjectVal
)

type gqlValueNode struct {
	kind   gqlValueKind
	num    float64
	str    string
	b      bool
	list   []*gqlValueNode
	fields []gqlObjectField
}

type gqlObjectField struct {
	name  string
	value *gqlValueNode
}

type gqlArgument struct {
	name  string
	value *gqlValueNode
}

type gqlDirective struct {
	name string
	args []gqlArgument
}

type gqlSelectionKind int

const (
	gqlFieldSel gqlSelectionKind = iota
	gqlSpreadSel
	gqlInlineSel
)

type gqlSelection struct {
	kind         gqlSelectionKind
	alias        string
	hasAlias     bool
	name         string
	args         []gqlArgument
	directives   []gqlDirective
	selectionSet []*gqlSelection
	hasSelSet    bool
	// For spreads and inline fragments.
	fragName      string
	typeCondition string
	hasTypeCond   bool
}

type gqlVarDef struct {
	name         string
	defaultValue *gqlValueNode
}

type gqlOperation struct {
	operation   string // "query" | "mutation" | "subscription"
	name        string
	hasName     bool
	variables   []gqlVarDef
	selectionSet []*gqlSelection
}

type gqlFragment struct {
	name         string
	typeCondition string
	selectionSet []*gqlSelection
}

type gqlDocument struct {
	operations []gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	index  int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.index] }

func (p *gqlParser) next() gqlToken {
	tok := p.tokens[p.index]
	if tok.kind != gqlEOF {
		p.index++
	}
	return tok
}

func (p *gqlParser) expectPunct(value string) error {
	tok := p.next()
	if tok.kind != gqlPunct || tok.value != value {
		got := tok.value
		if got == "" {
			got = "end of document"
		}
		return gqlSyntax("Expected %q, got %s", value, got)
	}
	return nil
}

func (p *gqlParser) expectName(value ...string) (string, error) {
	tok := p.next()
	want := ""
	if len(value) > 0 {
		want = value[0]
	}
	if tok.kind != gqlName || (len(value) > 0 && tok.value != want) {
		got := tok.value
		if got == "" {
			got = "end of document"
		}
		return "", gqlSyntax("Expected name %s, got %s", want, got)
	}
	return tok.value, nil
}

func (p *gqlParser) skipIfPunct(value string) bool {
	tok := p.peek()
	if tok.kind == gqlPunct && tok.value == value {
		p.index++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*gqlFragment{}}
	for p.peek().kind != gqlEOF {
		tok := p.peek()
		if tok.kind == gqlPunct && tok.value == "{" {
			selSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, gqlOperation{operation: "query", selectionSet: selSet})
		} else if tok.kind == gqlName && (tok.value == "query" || tok.value == "mutation" || tok.value == "subscription") {
			operation := p.next().value
			op := gqlOperation{operation: operation}
			if p.peek().kind == gqlName {
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				op.name, op.hasName = name, true
			}
			variables, err := p.parseVariableDefinitions()
			if err != nil {
				return nil, err
			}
			op.variables = variables
			if _, err := p.parseDirectives(); err != nil {
				return nil, err
			}
			selSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			op.selectionSet = selSet
			doc.operations = append(doc.operations, op)
		} else if tok.kind == gqlName && tok.value == "fragment" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.expectName("on"); err != nil {
				return nil, err
			}
			typeCond, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if _, err := p.parseDirectives(); err != nil {
				return nil, err
			}
			selSet, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = &gqlFragment{name: name, typeCondition: typeCond, selectionSet: selSet}
		} else {
			return nil, gqlSyntax("Unexpected token %s at %d", tok.value, tok.position)
		}
	}
	return doc, nil
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	var defs []gqlVarDef
	if !p.skipIfPunct("(") {
		return defs, nil
	}
	for !p.skipIfPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if err := p.parseType(); err != nil {
			return nil, err
		}
		def := gqlVarDef{name: name}
		if p.skipIfPunct("=") {
			defValue, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			def.defaultValue = defValue
		}
		defs = append(defs, def)
	}
	return defs, nil
}

func (p *gqlParser) parseType() error {
	if p.skipIfPunct("[") {
		if err := p.parseType(); err != nil {
			return err
		}
		return p.expectPunct("]")
	}
	if _, err := p.expectName(); err != nil {
		return err
	}
	p.skipIfPunct("!")
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []*gqlSelection
	for !p.skipIfPunct("}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
	return selections, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	tok := p.peek()
	if tok.kind == gqlPunct && tok.value == "..." {
		p.next()
		next := p.peek()
		if next.kind == gqlName && next.value != "on" {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			directives, err := p.parseDirectives()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{kind: gqlSpreadSel, fragName: name, directives: directives}, nil
		}
		sel := &gqlSelection{kind: gqlInlineSel}
		if next.kind == gqlName && next.value == "on" {
			p.next()
			typeCond, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sel.typeCondition, sel.hasTypeCond = typeCond, true
		}
		directives, err := p.parseDirectives()
		if err != nil {
			return nil, err
		}
		sel.directives = directives
		selSet, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selectionSet, sel.hasSelSet = selSet, true
		return sel, nil
	}
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	sel := &gqlSelection{kind: gqlFieldSel, name: name}
	if p.skipIfPunct(":") {
		sel.alias, sel.hasAlias = name, true
		realName, err := p.expectName()
		if err != nil {
			return nil, err
		}
		sel.name = realName
	}
	args, err := p.parseArguments()
	if err != nil {
		return nil, err
	}
	sel.args = args
	directives, err := p.parseDirectives()
	if err != nil {
		return nil, err
	}
	sel.directives = directives
	if p.peek().kind == gqlPunct && p.peek().value == "{" {
		selSet, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sel.selectionSet, sel.hasSelSet = selSet, true
	}
	return sel, nil
}

func (p *gqlParser) parseArguments() ([]gqlArgument, error) {
	var args []gqlArgument
	if !p.skipIfPunct("(") {
		return args, nil
	}
	for !p.skipIfPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArgument{name: name, value: value})
	}
	return args, nil
}

func (p *gqlParser) parseDirectives() ([]gqlDirective, error) {
	var directives []gqlDirective
	for p.skipIfPunct("@") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		directives = append(directives, gqlDirective{name: name, args: args})
	}
	return directives, nil
}

func (p *gqlParser) parseValue() (*gqlValueNode, error) {
	tok := p.next()
	if tok.kind == gqlPunct && tok.value == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &gqlValueNode{kind: gqlVar, str: name}, nil
	}
	if tok.kind == gqlInt {
		num, _ := strconv.ParseFloat(tok.value, 64)
		return &gqlValueNode{kind: gqlIntVal, num: num}, nil
	}
	if tok.kind == gqlFloat {
		num, _ := strconv.ParseFloat(tok.value, 64)
		return &gqlValueNode{kind: gqlFloatVal, num: num}, nil
	}
	if tok.kind == gqlString {
		return &gqlValueNode{kind: gqlStringVal, str: tok.value}, nil
	}
	if tok.kind == gqlPunct && tok.value == "[" {
		node := &gqlValueNode{kind: gqlListVal}
		for !(p.peek().kind == gqlPunct && p.peek().value == "]") {
			entry, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			node.list = append(node.list, entry)
		}
		p.next()
		return node, nil
	}
	if tok.kind == gqlPunct && tok.value == "{" {
		node := &gqlValueNode{kind: gqlObjectVal}
		for !(p.peek().kind == gqlPunct && p.peek().value == "}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			node.fields = append(node.fields, gqlObjectField{name: name, value: value})
		}
		p.next()
		return node, nil
	}
	if tok.kind == gqlName {
		switch tok.value {
		case "true":
			return &gqlValueNode{kind: gqlBoolVal, b: true}, nil
		case "false":
			return &gqlValueNode{kind: gqlBoolVal, b: false}, nil
		case "null":
			return &gqlValueNode{kind: gqlNullVal}, nil
		default:
			return &gqlValueNode{kind: gqlEnumVal, str: tok.value}, nil
		}
	}
	return nil, gqlSyntax("Unexpected value token %s", tok.value)
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// gqlValue is a resolved object: its GraphQL type name plus the store
// record behind it.
type gqlValue struct {
	typename string
	record   any
}

// gqlConn is a connection value: its page of nodes plus its page info.
type gqlConn struct {
	nodes           []*gqlValue
	hasNextPage     bool
	endCursor       *string
	hasPreviousPage bool
}

type gqlTypeInfo struct {
	name       string
	fields     []string
	enumValues []string
}

type gqlNamed struct{ name string }

type gqlFile struct{ path string }

type gqlItemPayload struct{ item *Item }

type gqlCommentPayload struct{ comment *Comment }

type gqlContext struct {
	store     *Store
	variables map[string]*JVal
	fragments map[string]*gqlFragment
	errors    []string
}

// gqlValueOf resolves a value node against the variables: a missing
// variable reads as null, an enum reads as its name, lists and objects
// resolve recursively.
func gqlValueOf(node *gqlValueNode, variables map[string]*JVal) *JVal {
	switch node.kind {
	case gqlVar:
		if v, ok := variables[node.str]; ok {
			return v
		}
		return JNul()
	case gqlIntVal, gqlFloatVal:
		return JNumRaw(formatJSNumber(node.num))
	case gqlStringVal:
		return JStr(node.str)
	case gqlBoolVal:
		return JBool(node.b)
	case gqlNullVal:
		return JNul()
	case gqlEnumVal:
		return JStr(node.str)
	case gqlListVal:
		arr := JArr()
		for _, entry := range node.list {
			arr.Arr = append(arr.Arr, gqlValueOf(entry, variables))
		}
		return arr
	case gqlObjectVal:
		obj := JObj()
		for _, field := range node.fields {
			obj.Set(field.name, gqlValueOf(field.value, variables))
		}
		return obj
	}
	return JNul()
}

// formatJSNumber renders a float64 the way JavaScript's String() and
// JSON.stringify render a number: the shortest round-trip spelling, with
// integers plain and very large or small magnitudes in exponent form.
func formatJSNumber(f float64) string {
	if f == 0 {
		// JavaScript prints -0 as "0" in JSON.stringify.
		return "0"
	}
	// strconv.FormatFloat with 'g' and -1 precision matches JavaScript's
	// shortest round-trip digits; the exponent thresholds differ, so
	// reformat through the shortest decimal expansion when needed.
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if strings.ContainsAny(s, "eE") {
		// JavaScript uses exponent form only outside [1e-6, 1e21).
		// Go's 'g' switches at 1e21 too but uses 1e-4 for small values,
		// so expand small exponents back to plain decimals.
		abs := f
		if abs < 0 {
			abs = -abs
		}
		if abs < 1e21 && abs >= 1e-6 {
			s = strconv.FormatFloat(f, 'f', -1, 64)
		} else {
			// Normalize Go's "1e+21" to JavaScript's "1e+21" (same)
			// and "1e-07" to "1e-7" (JavaScript drops the zero).
			s = normalizeJSExponent(s)
		}
	}
	return s
}

func normalizeJSExponent(s string) string {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return s
	}
	mantissa, exp := s[:i], s[i+1:]
	sign := ""
	if len(exp) > 0 && (exp[0] == '+' || exp[0] == '-') {
		sign = string(exp[0])
		exp = exp[1:]
	}
	exp = strings.TrimLeft(exp, "0")
	if exp == "" {
		exp = "0"
	}
	return mantissa + "e" + sign + exp
}

func gqlArgsOf(field *gqlSelection, variables map[string]*JVal) map[string]*JVal {
	args := map[string]*JVal{}
	for _, arg := range field.args {
		args[arg.name] = gqlValueOf(arg.value, variables)
	}
	return args
}

// gqlIsSkipped applies @skip(if:true) and @include(if:not true); every
// other directive is ignored. Only a strict boolean true counts.
func gqlIsSkipped(directives []gqlDirective, variables map[string]*JVal) bool {
	for _, directive := range directives {
		var ifArg *gqlArgument
		for i := range directive.args {
			if directive.args[i].name == "if" {
				ifArg = &directive.args[i]
				break
			}
		}
		isTrue := false
		if ifArg != nil {
			if v := gqlValueOf(ifArg.value, variables); v.Kind == KBool && v.Bool {
				isTrue = true
			}
		}
		if directive.name == "skip" && isTrue {
			return true
		}
		if directive.name == "include" && !isTrue {
			return true
		}
	}
	return false
}

func gqlAsString(v *JVal) (string, bool) {
	if v == nil {
		return "", false
	}
	return v.AsStr()
}

// gqlAsInt reads an integer argument: a safe-integer JSON number, or a
// digit string parsed the way parseInt parses it.
func gqlAsInt(v *JVal) (float64, bool) {
	if v == nil {
		return 0, false
	}
	if v.Kind == KNum {
		if i, ok := v.AsInt(); ok {
			return float64(i), true
		}
		return 0, false
	}
	if v.Kind == KStr {
		if !isDigitString(v.Str) {
			return 0, false
		}
		return parseDigitStringFloat(v.Str), true
	}
	return 0, false
}

func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i = 1
	}
	if i >= len(s) {
		return false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func parseDigitStringFloat(s string) float64 {
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	acc := 0.0
	for i := 0; i < len(s); i++ {
		acc = acc*10 + float64(s[i]-'0')
	}
	if neg {
		acc = -acc
	}
	return acc
}

func gqlAsRecord(v *JVal) *JVal {
	if v != nil && v.Kind == KObj {
		return v
	}
	return nil
}

// gqlStringify renders a JSON value the way JavaScript's String() does, for
// the global-id lookups that stringify their input first.
func gqlStringify(v *JVal) string {
	if v == nil {
		return "undefined"
	}
	switch v.Kind {
	case KNull:
		return "null"
	case KBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case KNum:
		if f, ok := v.AsFloat(); ok {
			return formatJSNumber(f)
		}
		return v.Num
	case KStr:
		return v.Str
	case KArr:
		parts := make([]string, len(v.Arr))
		for i, entry := range v.Arr {
			if entry.Kind == KNull || entry.Kind == KObj {
				parts[i] = ""
				continue
			}
			parts[i] = gqlStringify(entry)
		}
		return strings.Join(parts, ",")
	case KObj:
		return "[object Object]"
	}
	return ""
}

func gqlIssueState(item *Item) string {
	if item.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlPullState(item *Item) string {
	if item.State == "open" {
		return "OPEN"
	}
	if item.Merged {
		return "MERGED"
	}
	return "CLOSED"
}

func gqlMergeableState(store *Store, repo *Repo, item *Item) (string, error) {
	if item.State != "open" {
		return "UNKNOWN", nil
	}
	mergeable, err := store.IsMergeable(repo, item)
	if err != nil {
		return "", err
	}
	if mergeable {
		return "MERGEABLE", nil
	}
	return "CONFLICTING", nil
}

func gqlUserValue() *gqlValue  { return &gqlValue{typename: "User", record: nil} }
func gqlRepoValue(repo *Repo) *gqlValue { return &gqlValue{typename: "Repository", record: repo} }

func gqlItemValue(item *Item) *gqlValue {
	typename := "Issue"
	if item.Kind == KindPull {
		typename = "PullRequest"
	}
	return &gqlValue{typename: typename, record: item}
}

func gqlCommentValue(comment *Comment) *gqlValue {
	return &gqlValue{typename: "IssueComment", record: comment}
}

// gqlOrderedItems orders items by number for a connection: CREATED_AT,
// UPDATED_AT and COMMENTS all reduce to number order; only the direction
// matters.
func gqlOrderedItems(items []*Item, orderBy *JVal) []*Item {
	direction := "DESC"
	if record := gqlAsRecord(orderBy); record != nil {
		if dir, ok := record.Get("direction").AsStr(); ok {
			direction = dir
		}
	}
	sorted := append([]*Item(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	if direction != "ASC" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

func gqlStatesFilter(states *JVal) map[string]bool {
	if states == nil || states.Kind != KArr {
		return nil
	}
	wanted := map[string]bool{}
	for _, entry := range states.Arr {
		if s, ok := entry.AsStr(); ok {
			wanted[s] = true
		}
	}
	return wanted
}

// gqlPaginateConnection applies cursor pagination over an ordered list:
// the cursor names the last number of the previous page.
func gqlPaginateConnection(ordered []*Item, first *JVal, after *JVal) (nodes []*Item, hasNextPage bool, endCursor *string) {
	start := 0
	if cursor, ok := gqlAsString(after); ok && strings.HasPrefix(cursor, "cursor:") {
		if number, ok := parseLeadingInt(cursor[len("cursor:"):]); ok {
			for idx, entry := range ordered {
				if float64(entry.Number) == number {
					start = idx + 1
					break
				}
			}
		}
	}
	limit := len(ordered)
	if firstNum, ok := gqlAsInt(first); ok && firstNum >= 0 && firstNum < float64(len(ordered)) {
		limit = int(firstNum)
	}
	end := start + limit
	if end > len(ordered) {
		end = len(ordered)
	}
	if start > len(ordered) {
		start = len(ordered)
	}
	nodes = ordered[start:end]
	hasNextPage = start+len(nodes) < len(ordered)
	if len(nodes) > 0 {
		cursor := "cursor:" + strconv.Itoa(nodes[len(nodes)-1].Number)
		endCursor = &cursor
	}
	return nodes, hasNextPage, endCursor
}

func gqlConnectionValue(typename string, nodes []*gqlValue, hasNextPage bool, endCursor *string, after *JVal, afterPresent bool) *gqlValue {
	hasPrevious := afterPresent && after != nil && after.Kind != KNull
	return &gqlValue{typename: typename, record: &gqlConn{
		nodes:           nodes,
		hasNextPage:     hasNextPage,
		endCursor:       endCursor,
		hasPreviousPage: hasPrevious,
	}}
}

// ---------------------------------------------------------------------------
// Introspection tables
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "body", "state", "author", "labels", "comments", "createdAt", "updatedAt", "closedAt", "repository", "url"},
	"PullRequest": {"id", "number", "title", "body", "state", "headRefName", "baseRefName", "mergeable", "merged", "files", "author", "labels", "comments", "createdAt", "updatedAt", "closedAt", "mergedAt", "repository", "url"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "updatedAt", "url"},
	"User":        {"id", "login", "name", "email"},
}

var gqlEnumValues = map[string][]string{
	"SearchType":           {"ISSUE", "REPOSITORY", "USER", "COMMIT", "CODE", "DISCUSSION", "LABEL", "MILESTONE", "PACKAGE", "WIKI"},
	"IssueState":           {"OPEN", "CLOSED"},
	"PullRequestState":     {"OPEN", "CLOSED", "MERGED"},
	"MergeableState":       {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
	"OrderDirection":       {"ASC", "DESC"},
	"IssueOrderField":      {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
}

// ---------------------------------------------------------------------------
// Field resolution
// ---------------------------------------------------------------------------

// gqlFieldResult is what resolveField returns: a JSON scalar, a typed
// value, an array of typed values, or the unknown-field sentinel.
type gqlFieldResult struct {
	scalar  *JVal
	typed   *gqlValue
	list    []*gqlValue
	unknown bool
}

func gqlScalar(v *JVal) gqlFieldResult       { return gqlFieldResult{scalar: v} }
func gqlTyped(v *gqlValue) gqlFieldResult    { return gqlFieldResult{typed: v} }
func gqlList(v []*gqlValue) gqlFieldResult   { return gqlFieldResult{list: v} }
func gqlUnknownField() gqlFieldResult        { return gqlFieldResult{unknown: true} }
func gqlNull() gqlFieldResult                { return gqlFieldResult{scalar: JNul()} }

// resolveField resolves one field on a typed value. An unknown field
// returns the unknown sentinel; the caller records the error and omits the
// field. A nil parent means a root field.
func resolveField(ctx *gqlContext, parent *gqlValue, fieldName string, args map[string]*JVal) (gqlFieldResult, error) {
	store := ctx.store

	// Root fields (Query / Mutation) have no parent.
	if parent == nil {
		switch fieldName {
		case "viewer":
			return gqlTyped(gqlUserValue()), nil
		case "__type":
			name, ok := gqlAsString(args["name"])
			if !ok {
				return gqlNull(), nil
			}
			fields, hasFields := gqlTypeFields[name]
			enumValues, hasEnums := gqlEnumValues[name]
			if !hasFields && !hasEnums {
				return gqlNull(), nil
			}
			if fields == nil {
				fields = []string{}
			}
			if enumValues == nil {
				enumValues = []string{}
			}
			return gqlTyped(&gqlValue{typename: "__Type", record: &gqlTypeInfo{name: name, fields: fields, enumValues: enumValues}}), nil
		case "repository":
			owner, ok1 := gqlAsString(args["owner"])
			name, ok2 := gqlAsString(args["name"])
			if !ok1 || !ok2 {
				return gqlNull(), nil
			}
			repo := store.GetRepo(owner, name)
			if repo == nil {
				return gqlNull(), nil
			}
			return gqlTyped(gqlRepoValue(repo)), nil
		case "node":
			id, ok := gqlAsString(args["id"])
			if !ok {
				return gqlNull(), nil
			}
			kind, repo, item, comment := store.NodeByGlobalID(id)
			switch kind {
			case NodeRepo:
				return gqlTyped(gqlRepoValue(repo)), nil
			case NodeItem:
				return gqlTyped(gqlItemValue(item)), nil
			case NodeComment:
				return gqlTyped(gqlCommentValue(comment)), nil
			}
			return gqlNull(), nil
		case "createIssue":
			return gqlCreateIssue(ctx, args)
		case "closeIssue":
			return gqlCloseIssue(ctx, args)
		case "createPullRequest":
			return gqlCreatePullRequest(ctx, args)
		case "mergePullRequest":
			return gqlMergePullRequest(ctx, args)
		case "addComment":
			return gqlAddComment(ctx, args)
		}
		return gqlUnknownField(), nil
	}

	switch parent.typename {
	case "__Type":
		record := parent.record.(*gqlTypeInfo)
		switch fieldName {
		case "name":
			return gqlScalar(JStr(record.name)), nil
		case "fields":
			var list []*gqlValue
			for _, name := range record.fields {
				list = append(list, &gqlValue{typename: "__Field", record: &gqlNamed{name: name}})
			}
			return gqlList(list), nil
		case "enumValues":
			var list []*gqlValue
			for _, name := range record.enumValues {
				list = append(list, &gqlValue{typename: "__EnumValue", record: &gqlNamed{name: name}})
			}
			return gqlList(list), nil
		}
		return gqlUnknownField(), nil
	case "__Field", "__EnumValue":
		if fieldName == "name" {
			return gqlScalar(JStr(parent.record.(*gqlNamed).name)), nil
		}
		return gqlUnknownField(), nil
	case "User":
		switch fieldName {
		case "login":
			return gqlScalar(JStr(viewerLogin)), nil
		case "id":
			return gqlScalar(JStr(fmt.Sprintf("U_%d", viewerID))), nil
		case "name":
			return gqlScalar(JStr(viewerName)), nil
		case "email":
			return gqlScalar(JStr(viewerEmail)), nil
		}
		return gqlUnknownField(), nil
	case "Ref":
		if fieldName == "name" {
			return gqlScalar(JStr(parent.record.(*gqlNamed).name)), nil
		}
		return gqlUnknownField(), nil
	case "Repository":
		return resolveRepositoryField(ctx, parent.record.(*Repo), fieldName, args)
	case "Issue", "PullRequest":
		return resolveItemField(ctx, parent, fieldName, args)
	case "PullRequestChangedFile":
		if fieldName == "path" {
			return gqlScalar(JStr(parent.record.(*gqlFile).path)), nil
		}
		return gqlUnknownField(), nil
	case "IssueComment":
		comment := parent.record.(*Comment)
		switch fieldName {
		case "id":
			return gqlScalar(JStr(globalCommentID(comment))), nil
		case "body":
			return gqlScalar(JStr(comment.Body)), nil
		case "createdAt":
			return gqlScalar(JStr(comment.CreatedAt)), nil
		case "updatedAt":
			return gqlScalar(JStr(comment.UpdatedAt)), nil
		case "author":
			return gqlTyped(gqlUserValue()), nil
		}
		return gqlUnknownField(), nil
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		conn := parent.record.(*gqlConn)
		switch fieldName {
		case "nodes":
			return gqlList(conn.nodes), nil
		case "pageInfo":
			return gqlTyped(&gqlValue{typename: "PageInfo", record: conn}), nil
		case "totalCount":
			return gqlScalar(JInt(int64(len(conn.nodes)))), nil
		}
		return gqlUnknownField(), nil
	case "PageInfo":
		conn := parent.record.(*gqlConn)
		switch fieldName {
		case "hasNextPage":
			return gqlScalar(JBool(conn.hasNextPage)), nil
		case "hasPreviousPage":
			return gqlScalar(JBool(conn.hasPreviousPage)), nil
		case "endCursor":
			if conn.endCursor == nil {
				return gqlNull(), nil
			}
			return gqlScalar(JStr(*conn.endCursor)), nil
		case "startCursor":
			return gqlNull(), nil
		}
		return gqlUnknownField(), nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if fieldName == "issue" {
			return gqlTyped(gqlItemValue(parent.record.(*gqlItemPayload).item)), nil
		}
		return gqlUnknownField(), nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if fieldName == "pullRequest" {
			return gqlTyped(gqlItemValue(parent.record.(*gqlItemPayload).item)), nil
		}
		return gqlUnknownField(), nil
	case "AddCommentPayload":
		if fieldName == "commentEdge" {
			return gqlTyped(&gqlValue{typename: "IssueCommentEdge", record: parent.record}), nil
		}
		return gqlUnknownField(), nil
	case "IssueCommentEdge":
		if fieldName == "node" {
			return gqlTyped(gqlCommentValue(parent.record.(*gqlCommentPayload).comment)), nil
		}
		return gqlUnknownField(), nil
	}
	return gqlUnknownField(), nil
}

func resolveRepositoryField(ctx *gqlContext, repo *Repo, fieldName string, args map[string]*JVal) (gqlFieldResult, error) {
	store := ctx.store
	switch fieldName {
	case "id":
		return gqlScalar(JStr(globalRepoID(repo))), nil
	case "databaseId":
		return gqlScalar(JInt(int64(repo.ID))), nil
	case "name":
		return gqlScalar(JStr(repo.Name)), nil
	case "nameWithOwner":
		return gqlScalar(JStr(repo.FullName)), nil
	case "description":
		return gqlScalar(JStr(repo.Description)), nil
	case "isPrivate":
		return gqlScalar(JBool(repo.IsPrivate)), nil
	case "createdAt":
		return gqlScalar(JStr(repo.CreatedAt)), nil
	case "updatedAt":
		return gqlScalar(JStr(repo.UpdatedAt)), nil
	case "owner":
		return gqlTyped(gqlUserValue()), nil
	case "defaultBranchRef":
		if _, ok := store.GetRef(repo, "refs/heads/"+repo.DefaultBranch); !ok {
			return gqlNull(), nil
		}
		return gqlTyped(&gqlValue{typename: "Ref", record: &gqlNamed{name: repo.DefaultBranch}}), nil
	case "viewerPermission":
		return gqlScalar(JStr("ADMIN")), nil
	case "hasIssuesEnabled":
		return gqlScalar(JBool(true)), nil
	case "issue", "pullRequest":
		number, ok := gqlAsInt(args["number"])
		if !ok {
			return gqlNull(), nil
		}
		if number < 1 || number > 1<<30 {
			return gqlNull(), nil
		}
		item := store.GetItem(repo, int(number))
		wantKind := KindIssue
		if fieldName == "pullRequest" {
			wantKind = KindPull
		}
		if item == nil || item.Kind != wantKind {
			return gqlNull(), nil
		}
		return gqlTyped(gqlItemValue(item)), nil
	case "issues", "pullRequests":
		kind := KindIssue
		connName := "IssueConnection"
		if fieldName == "pullRequests" {
			kind = KindPull
			connName = "PullRequestConnection"
		}
		var items []*Item
		for _, item := range store.ListItems(repo) {
			if item.Kind == kind {
				items = append(items, item)
			}
		}
		if wanted := gqlStatesFilter(args["states"]); wanted != nil {
			filtered := items[:0]
			for _, item := range items {
				state := gqlIssueState(item)
				if kind == KindPull {
					state = gqlPullState(item)
				}
				if wanted[state] {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		} else {
			filtered := items[:0]
			for _, item := range items {
				if item.State == "open" {
					filtered = append(filtered, item)
				}
			}
			items = filtered
		}
		ordered := gqlOrderedItems(items, args["orderBy"])
		after, afterPresent := args["after"]
		nodes, hasNext, endCursor := gqlPaginateConnection(ordered, args["first"], after)
		var typedNodes []*gqlValue
		for _, node := range nodes {
			typedNodes = append(typedNodes, gqlItemValue(node))
		}
		return gqlTyped(gqlConnectionValue(connName, typedNodes, hasNext, endCursor, after, afterPresent)), nil
	}
	return gqlUnknownField(), nil
}

func resolveItemField(ctx *gqlContext, parent *gqlValue, fieldName string, args map[string]*JVal) (gqlFieldResult, error) {
	store := ctx.store
	item := parent.record.(*Item)
	repo := store.RepoOfItem(item)
	switch fieldName {
	case "id":
		return gqlScalar(JStr(globalItemID(item))), nil
	case "number":
		return gqlScalar(JInt(int64(item.Number))), nil
	case "title":
		return gqlScalar(JStr(item.Title)), nil
	case "body":
		return gqlScalar(JStr(item.Body)), nil
	case "state":
		if parent.typename == "Issue" {
			return gqlScalar(JStr(gqlIssueState(item))), nil
		}
		return gqlScalar(JStr(gqlPullState(item))), nil
	case "createdAt":
		return gqlScalar(JStr(item.CreatedAt)), nil
	case "updatedAt":
		return gqlScalar(JStr(item.UpdatedAt)), nil
	case "closedAt":
		if item.ClosedAt == "" {
			return gqlNull(), nil
		}
		return gqlScalar(JStr(item.ClosedAt)), nil
	case "author":
		return gqlTyped(gqlUserValue()), nil
	case "repository":
		if repo == nil {
			return gqlNull(), nil
		}
		return gqlTyped(gqlRepoValue(repo)), nil
	}
	if parent.typename == "PullRequest" {
		switch fieldName {
		case "headRefName":
			return gqlScalar(JStr(item.HeadRef)), nil
		case "baseRefName":
			return gqlScalar(JStr(item.BaseRef)), nil
		case "merged":
			return gqlScalar(JBool(item.Merged)), nil
		case "mergedAt":
			if item.MergedAt == "" {
				return gqlNull(), nil
			}
			return gqlScalar(JStr(item.MergedAt)), nil
		case "mergeable":
			if repo == nil {
				return gqlScalar(JStr("UNKNOWN")), nil
			}
			state, err := gqlMergeableState(store, repo, item)
			if err != nil {
				return gqlFieldResult{}, err
			}
			return gqlScalar(JStr(state)), nil
		case "files":
			empty := func() gqlFieldResult {
				return gqlTyped(gqlConnectionValue("PullRequestChangedFileConnection", nil, false, nil, nil, false))
			}
			if repo == nil {
				return empty(), nil
			}
			var baseTip, headTip string
			var baseOK, headOK bool
			var err error
			if item.BaseRef != "" {
				baseTip, baseOK, err = store.ResolveRef(repo, item.BaseRef)
				if err != nil {
					return gqlFieldResult{}, err
				}
			}
			if item.HeadRef != "" {
				headTip, headOK, err = store.ResolveRef(repo, item.HeadRef)
				if err != nil {
					return gqlFieldResult{}, err
				}
			}
			if !baseOK || !headOK {
				return empty(), nil
			}
			base, hasBase := store.MergeBase(repo, baseTip, headTip)
			files := store.DiffFiles(repo, base, hasBase, headTip)
			if first, ok := gqlAsInt(args["first"]); ok {
				if first < 0 {
					files = files[:0]
				} else if first < float64(len(files)) {
					files = files[:int(first)]
				}
			}
			var nodes []*gqlValue
			for _, file := range files {
				nodes = append(nodes, &gqlValue{typename: "PullRequestChangedFile", record: &gqlFile{path: file.Filename}})
			}
			return gqlTyped(gqlConnectionValue("PullRequestChangedFileConnection", nodes, false, nil, nil, false)), nil
		}
	}
	return gqlUnknownField(), nil
}

// ---------------------------------------------------------------------------
// Mutations
// ---------------------------------------------------------------------------

func gqlMutationError(ctx *gqlContext, message string) (gqlFieldResult, error) {
	ctx.errors = append(ctx.errors, message)
	return gqlNull(), nil
}

func gqlCreateIssue(ctx *gqlContext, args map[string]*JVal) (gqlFieldResult, error) {
	input := gqlAsRecord(args["input"])
	if input == nil {
		return gqlMutationError(ctx, "createIssue requires an input object")
	}
	repoID, present := input.Get("repositoryId"), input.Has("repositoryId")
	var repo *Repo
	if present {
		repo = ctx.store.RepoByGlobalID(gqlStringify(repoID))
	}
	if repo == nil {
		return gqlMutationError(ctx, "createIssue: repository not found")
	}
	title, ok := input.Get("title").AsStr()
	if !ok || title == "" {
		return gqlMutationError(ctx, "createIssue: title is required")
	}
	body, _ := input.Get("body").AsStr()
	item, err := ctx.store.CreateItem(repo, KindIssue, title, body, nil, "", "")
	if err != nil {
		if se, ok := err.(*StoreError); ok {
			return gqlMutationError(ctx, se.Message)
		}
		return gqlMutationError(ctx, "createIssue failed")
	}
	return gqlTyped(&gqlValue{typename: "CreateIssuePayload", record: &gqlItemPayload{item: item}}), nil
}

func gqlCloseIssue(ctx *gqlContext, args map[string]*JVal) (gqlFieldResult, error) {
	input := gqlAsRecord(args["input"])
	if input == nil {
		return gqlMutationError(ctx, "closeIssue requires an input object")
	}
	var repo *Repo
	var item *Item
	if input.Has("issueId") {
		repo, item = ctx.store.ItemByGlobalID(gqlStringify(input.Get("issueId")))
	}
	if item == nil {
		return gqlMutationError(ctx, "closeIssue: issue not found")
	}
	item.State = "closed"
	item.ClosedAt = now.iso
	item.UpdatedAt = now.iso
	ctx.store.Touch(repo)
	return gqlTyped(&gqlValue{typename: "CloseIssuePayload", record: &gqlItemPayload{item: item}}), nil
}

func gqlCreatePullRequest(ctx *gqlContext, args map[string]*JVal) (gqlFieldResult, error) {
	input := gqlAsRecord(args["input"])
	if input == nil {
		return gqlMutationError(ctx, "createPullRequest requires an input object")
	}
	var repo *Repo
	if input.Has("repositoryId") {
		repo = ctx.store.RepoByGlobalID(gqlStringify(input.Get("repositoryId")))
	}
	if repo == nil {
		return gqlMutationError(ctx, "createPullRequest: repository not found")
	}
	title, ok := input.Get("title").AsStr()
	head, headOK := input.Get("headRefName").AsStr()
	base, baseOK := input.Get("baseRefName").AsStr()
	if !ok || title == "" {
		return gqlMutationError(ctx, "createPullRequest: title is required")
	}
	if !headOK {
		return gqlMutationError(ctx, "createPullRequest: head branch not found")
	}
	if _, found, err := ctx.store.ResolveRef(repo, head); err != nil {
		return gqlFieldResult{}, err
	} else if !found {
		return gqlMutationError(ctx, "createPullRequest: head branch not found")
	}
	if !baseOK {
		return gqlMutationError(ctx, "createPullRequest: base branch not found")
	}
	if _, found, err := ctx.store.ResolveRef(repo, base); err != nil {
		return gqlFieldResult{}, err
	} else if !found {
		return gqlMutationError(ctx, "createPullRequest: base branch not found")
	}
	body, _ := input.Get("body").AsStr()
	item, err := ctx.store.CreateItem(repo, KindPull, title, body, nil, head, base)
	if err != nil {
		if se, ok := err.(*StoreError); ok {
			return gqlMutationError(ctx, se.Message)
		}
		return gqlMutationError(ctx, "createPullRequest failed")
	}
	return gqlTyped(&gqlValue{typename: "CreatePullRequestPayload", record: &gqlItemPayload{item: item}}), nil
}

func gqlMergePullRequest(ctx *gqlContext, args map[string]*JVal) (gqlFieldResult, error) {
	input := gqlAsRecord(args["input"])
	if input == nil {
		return gqlMutationError(ctx, "mergePullRequest requires an input object")
	}
	var repo *Repo
	var item *Item
	if input.Has("pullRequestId") {
		repo, item = ctx.store.ItemByGlobalID(gqlStringify(input.Get("pullRequestId")))
	}
	if item == nil || item.Kind != KindPull {
		return gqlMutationError(ctx, "mergePullRequest: pull request not found")
	}
	if item.State != "open" {
		return gqlMutationError(ctx, "mergePullRequest: pull request is already closed")
	}
	sha, err := ctx.store.MergePullRequest(repo, item, fmt.Sprintf("%s (#%d)", item.Title, item.Number))
	if err != nil {
		return gqlFieldResult{}, err
	}
	if sha == "" {
		return gqlMutationError(ctx, "mergePullRequest: pull request cannot be merged due to conflicts")
	}
	return gqlTyped(&gqlValue{typename: "MergePullRequestPayload", record: &gqlItemPayload{item: item}}), nil
}

func gqlAddComment(ctx *gqlContext, args map[string]*JVal) (gqlFieldResult, error) {
	input := gqlAsRecord(args["input"])
	if input == nil {
		return gqlMutationError(ctx, "addComment requires an input object")
	}
	var repo *Repo
	var item *Item
	if input.Has("subjectId") {
		repo, item = ctx.store.ItemByGlobalID(gqlStringify(input.Get("subjectId")))
	}
	if item == nil {
		return gqlMutationError(ctx, "addComment: subject not found")
	}
	body, ok := input.Get("body").AsStr()
	if !ok || body == "" {
		return gqlMutationError(ctx, "addComment: body is required")
	}
	comment := ctx.store.AddComment(repo, item, body)
	return gqlTyped(&gqlValue{typename: "AddCommentPayload", record: &gqlCommentPayload{comment: comment}}), nil
}

// ---------------------------------------------------------------------------
// Selection execution
// ---------------------------------------------------------------------------

func gqlExecuteSelectionSet(ctx *gqlContext, parent *gqlValue, parentTypename string, selections []*gqlSelection) (*JVal, error) {
	result := JObj()
	for _, selection := range selections {
		switch selection.kind {
		case gqlSpreadSel:
			if gqlIsSkipped(selection.directives, ctx.variables) {
				continue
			}
			fragment, ok := ctx.fragments[selection.fragName]
			if !ok || (fragment.typeCondition != parentTypename && fragment.typeCondition != "Node") {
				continue
			}
			merged, err := gqlExecuteSelectionSet(ctx, parent, parentTypename, fragment.selectionSet)
			if err != nil {
				return nil, err
			}
			for i, key := range merged.Keys {
				result.Set(key, merged.Vals[i])
			}
		case gqlInlineSel:
			if gqlIsSkipped(selection.directives, ctx.variables) {
				continue
			}
			if selection.hasTypeCond && selection.typeCondition != parentTypename && selection.typeCondition != "Node" {
				continue
			}
			merged, err := gqlExecuteSelectionSet(ctx, parent, parentTypename, selection.selectionSet)
			if err != nil {
				return nil, err
			}
			for i, key := range merged.Keys {
				result.Set(key, merged.Vals[i])
			}
		case gqlFieldSel:
			if gqlIsSkipped(selection.directives, ctx.variables) {
				continue
			}
			key := selection.name
			if selection.hasAlias {
				key = selection.alias
			}
			args := gqlArgsOf(selection, ctx.variables)
			value, err := resolveField(ctx, parent, selection.name, args)
			if err != nil {
				return nil, err
			}
			if value.unknown {
				ctx.errors = append(ctx.errors, fmt.Sprintf("Cannot query field %q on type %q.", selection.name, parentTypename))
				continue
			}
			var selSet []*gqlSelection
			hasSelSet := selection.hasSelSet
			if hasSelSet {
				selSet = selection.selectionSet
			}
			completed, err := gqlCompleteValue(ctx, value, selSet, hasSelSet)
			if err != nil {
				return nil, err
			}
			result.Set(key, completed)
		}
	}
	return result, nil
}

func gqlCompleteValue(ctx *gqlContext, value gqlFieldResult, selectionSet []*gqlSelection, hasSelSet bool) (*JVal, error) {
	if value.scalar != nil {
		return value.scalar, nil
	}
	if value.list != nil {
		arr := JArr()
		for _, entry := range value.list {
			completed, err := gqlCompleteValue(ctx, gqlTyped(entry), selectionSet, hasSelSet)
			if err != nil {
				return nil, err
			}
			arr.Arr = append(arr.Arr, completed)
		}
		return arr, nil
	}
	if value.typed != nil {
		if !hasSelSet {
			return JNul(), nil
		}
		return gqlExecuteSelectionSet(ctx, value.typed, value.typed.typename, selectionSet)
	}
	return JNul(), nil
}

func gqlExecuteOperation(store *Store, doc *gqlDocument, operationName *string, variables map[string]*JVal) (*JVal, []string, error) {
	var errors []string
	var operation *gqlOperation
	if operationName != nil {
		for i := range doc.operations {
			if doc.operations[i].hasName && doc.operations[i].name == *operationName {
				operation = &doc.operations[i]
				break
			}
		}
		if operation == nil {
			return nil, []string{fmt.Sprintf("Unknown operation named %q.", *operationName)}, nil
		}
	} else if len(doc.operations) == 1 {
		operation = &doc.operations[0]
	} else {
		return nil, []string{"Must provide an operation name when the document holds more than one operation."}, nil
	}
	resolvedVariables := map[string]*JVal{}
	for k, v := range variables {
		resolvedVariables[k] = v
	}
	for _, variable := range operation.variables {
		if _, present := resolvedVariables[variable.name]; !present && variable.defaultValue != nil {
			resolvedVariables[variable.name] = gqlValueOf(variable.defaultValue, resolvedVariables)
		}
	}
	ctx := &gqlContext{store: store, variables: resolvedVariables, fragments: doc.fragments}
	rootTypename := "Query"
	if operation.operation == "mutation" {
		rootTypename = "Mutation"
	}
	data, err := gqlExecuteSelectionSet(ctx, nil, rootTypename, operation.selectionSet)
	if err != nil {
		return nil, nil, err
	}
	errors = append(errors, ctx.errors...)
	return data, errors, nil
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func gqlExecuteOne(store *Store, entry *JVal) (*JVal, error) {
	record := gqlAsRecord(entry)
	if record == nil {
		return gqlEnvelope(JNul(), []string{"Must provide a query object."}), nil
	}
	queryVal := record.Get("query")
	query, ok := queryVal.AsStr()
	if !ok {
		return gqlEnvelope(JNul(), []string{"Must provide query string."}), nil
	}
	variables := map[string]*JVal{}
	if varsRecord := gqlAsRecord(record.Get("variables")); varsRecord != nil {
		for i, key := range varsRecord.Keys {
			variables[key] = varsRecord.Vals[i]
		}
	}
	var operationName *string
	if name, ok := record.Get("operationName").AsStr(); ok {
		operationName = &name
	}
	tokens, err := gqlLex(query)
	if err != nil {
		return gqlEnvelope(JNul(), []string{err.Error()}), nil
	}
	doc, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return gqlEnvelope(JNul(), []string{err.Error()}), nil
	}
	data, errs, err := gqlExecuteOperation(store, doc, operationName, variables)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return gqlEnvelope(JNul(), errs), nil
	}
	return gqlEnvelope(data, errs), nil
}

func gqlEnvelope(data *JVal, errs []string) *JVal {
	envelope := JObj().Set("data", data)
	if len(errs) > 0 {
		arr := JArr()
		for _, message := range errs {
			arr.Arr = append(arr.Arr, JObj().Set("message", JStr(message)))
		}
		envelope.Set("errors", arr)
	}
	return envelope
}

// handleGraphQL answers one POST /api/graphql body (a single operation
// object, or a JSON array of them for batching). It always answers HTTP 200
// with GraphQL envelopes; transport-level problems (auth, malformed JSON)
// are the transport layer's 401/400.
func handleGraphQL(store *Store, body *JVal) (handlerReply, error) {
	headers := [][2]string{
		{"content-type", jsonType},
		{"x-github-request-id", store.NextRequestID()},
	}
	if body != nil && body.Kind == KArr {
		arr := JArr()
		for _, entry := range body.Arr {
			result, err := gqlExecuteOne(store, entry)
			if err != nil {
				return handlerReply{}, err
			}
			arr.Arr = append(arr.Arr, result)
		}
		bytes := AppendJSON(nil, arr)
		bytes = append(bytes, '\n')
		return handlerReply{status: 200, headers: headers, body: bytes}, nil
	}
	if gqlAsRecord(body) == nil {
		bytes := AppendJSON(nil, JObj().Set("message", JStr("Problems parsing JSON")))
		bytes = append(bytes, '\n')
		return handlerReply{status: 400, headers: headers, body: bytes}, nil
	}
	result, err := gqlExecuteOne(store, body)
	if err != nil {
		return handlerReply{}, err
	}
	bytes := AppendJSON(nil, result)
	bytes = append(bytes, '\n')
	return handlerReply{status: 200, headers: headers, body: bytes}, nil
}

// Ensure the errors import is used even when no other helper needs it.
var _ = errors.New
