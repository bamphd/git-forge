// The forge's HTTP front door: one server, one store, four surfaces.
// Routing is by path:
//
//	GET  /healthz                the env's boot check, no authentication
//	POST /mcp                    JSON-RPC 2.0
//	POST /api/graphql            GraphQL
//	*    /api/v3/...             REST v3
//	*    /{owner}/{repo}.git/... git smart HTTP
//
// Every route except /healthz requires the trial's bearer token (an exact
// match against MAKEFUN_SHOP_TOKEN when it is set; any non-empty bearer
// when it is not, so local runs without the variable still work while a
// missing token is always 401). Every JSON reply carries a non-empty
// X-GitHub-Request-Id and application/json; unknown routes are JSON 404s; a
// malformed JSON body is a 400; a request error never takes the server down.
//
// The transport is a raw TCP server speaking HTTP/1.1 directly: the
// benchmark workloads are thousands of small requests, and skipping the
// http module's stream machinery, per-request objects and header
// re-serialization is worth a fifth of the wall time. The parser is
// deliberately strict where the wire format is unambiguous (request line,
// header shape, content-length) and lenient where clients vary (LF-only
// line endings, folded whitespace), and it implements the framing the
// clients use: content-length bodies, chunked bodies, keep-alive,
// pipelining and Expect: 100-continue. Dispatch itself is synchronous and
// serialized: once a request's bytes are complete, its reply is built
// under one mutex and written before the next request on the socket is
// read, so responses always leave in request order.

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Bodies larger than this are refused; the suite's packs are far smaller.
const maxBodyBytes = 256 * 1024 * 1024

// Request head (line + headers) larger than this is refused, as the http
// module would.
const maxHeadBytes = 16 * 1024

// The interim answer to `Expect: 100-continue`, sent as soon as the head is
// parsed.
var continue100 = []byte("HTTP/1.1 100 Continue\r\n\r\n")

var healthzBody = []byte("{\"ok\":true}\n")

var reasons = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	431: "Request Header Fields Too Large",
	500: "Internal Server Error",
}

// ---------------------------------------------------------------------------
// Replies
// ---------------------------------------------------------------------------

// reply is one serialized answer: its status, header lines in wire order
// (without the trailing CRLF; content-length is appended), and its body.
type reply struct {
	status  int
	headers []string
	body    []byte
}

func jsonErrorReply(store *Store, status int, message string) reply {
	body := AppendJSON(nil, JObj().Set("message", JStr(message)))
	body = append(body, '\n')
	return reply{
		status: status,
		headers: []string{
			"content-type: application/json; charset=utf-8",
			"x-github-request-id: " + store.NextRequestID(),
		},
		body: body,
	}
}

// handlerReply is what a surface handler returns: a status, ordered
// response headers as name/value pairs, and a body (nil for an empty body).
type handlerReply struct {
	status  int
	headers [][2]string
	body    []byte
}

func toReply(hr handlerReply) reply {
	lines := make([]string, 0, len(hr.headers))
	for _, h := range hr.headers {
		lines = append(lines, h[0]+": "+h[1])
	}
	return reply{status: hr.status, headers: lines, body: hr.body}
}

// serializeReply renders one reply: status line, headers, content-length,
// blank line, body. headOnly answers a HEAD request with the head alone.
func serializeReply(r reply, close, headOnly bool) []byte {
	var b []byte
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(r.status), 10)
	b = append(b, ' ')
	b = append(b, reasons[r.status]...)
	b = append(b, "\r\n"...)
	for _, line := range r.headers {
		b = append(b, line...)
		b = append(b, "\r\n"...)
	}
	b = append(b, "content-length: "...)
	b = strconv.AppendInt(b, int64(len(r.body)), 10)
	b = append(b, "\r\n"...)
	if close {
		b = append(b, "connection: close\r\n"...)
	}
	b = append(b, "\r\n"...)
	if headOnly {
		return b
	}
	return append(b, r.body...)
}

// ---------------------------------------------------------------------------
// Query strings (URLSearchParams semantics)
// ---------------------------------------------------------------------------

type queryPair struct {
	key, value string
}

// queryParams is an ordered list of query pairs, parsed and serialized the
// way URLSearchParams does.
type queryParams struct {
	pairs []queryPair
}

// parseQueryParams parses an application/x-www-form-urlencoded query
// string: pairs split on '&', each split on the first '=', '+' meaning a
// space, percent-decoding lenient (a malformed sequence stays literal),
// the result decoded as UTF-8 with replacement.
func parseQueryParams(s string) queryParams {
	if s == "" {
		return queryParams{}
	}
	parts := strings.Split(s, "&")
	q := queryParams{pairs: make([]queryPair, 0, len(parts))}
	for _, part := range parts {
		name, value := part, ""
		if eq := strings.IndexByte(part, '='); eq >= 0 {
			name, value = part[:eq], part[eq+1:]
		}
		q.pairs = append(q.pairs, queryPair{
			key:   urlEncodedDecode(name),
			value: urlEncodedDecode(value),
		})
	}
	return q
}

func urlEncodedDecode(s string) string {
	var b []byte
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '+':
			b = append(b, ' ')
			i++
		case c == '%' && i+2 < len(s):
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if ok1 && ok2 {
				b = append(b, byte(hi<<4|lo))
				i += 3
			} else {
				b = append(b, '%')
				i++
			}
		default:
			b = append(b, c)
			i++
		}
	}
	return SanitizeUTF8(string(b))
}

// get returns the first value for name, or "" when absent.
func (q queryParams) get(name string) string {
	for _, p := range q.pairs {
		if p.key == name {
			return p.value
		}
	}
	return ""
}

// has reports whether name appears at all.
func (q queryParams) has(name string) bool {
	for _, p := range q.pairs {
		if p.key == name {
			return true
		}
	}
	return false
}

// set replaces every pair named name with one pair, at the first removed
// pair's position, or appends when the name is absent.
func (q *queryParams) set(name, value string) {
	at := -1
	kept := q.pairs[:0]
	for i, p := range q.pairs {
		if p.key == name {
			if at < 0 {
				at = i
			}
			continue
		}
		kept = append(kept, p)
	}
	q.pairs = kept
	if at < 0 {
		q.pairs = append(q.pairs, queryPair{name, value})
		return
	}
	// at indexes the original slice; the kept slice is shorter by the
	// number of removed pairs before at, which is zero (at is the first).
	q.pairs = append(q.pairs, queryPair{})
	copy(q.pairs[at+1:], q.pairs[at:])
	q.pairs[at] = queryPair{name, value}
}

// String serializes the pairs the way URLSearchParams does: alphanumerics
// and *-._ stay literal, a space becomes '+', anything else becomes an
// uppercase %XX escape of its UTF-8 bytes.
func (q queryParams) String() string {
	var b strings.Builder
	for i, p := range q.pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		writeURLEncoded(&b, p.key)
		b.WriteByte('=')
		writeURLEncoded(&b, p.value)
	}
	return b.String()
}

func writeURLEncoded(b *strings.Builder, s string) {
	const hexUp = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '*' || c == '-' || c == '.' || c == '_':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(hexUp[c>>4])
			b.WriteByte(hexUp[c&0xf])
		}
	}
}

// ---------------------------------------------------------------------------
// Request targets
// ---------------------------------------------------------------------------

// parseRequestTarget splits a request target into decoded path segments and
// a query, without a WHATWG URL parse: the suite's targets are origin-form
// paths, so a manual split is exact and far cheaper. An absolute-form
// target has its scheme and authority stripped; a fragment is dropped, as a
// URL parse would. Segments are percent-decoded only when they contain a
// '%', and a segment that fails to decode is kept raw.
func parseRequestTarget(target string) (segments []string, query queryParams) {
	path := target
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		rest := path[strings.Index(path, "://")+3:]
		if slash := strings.IndexByte(rest, '/'); slash < 0 {
			path = "/"
		} else {
			path = rest[slash:]
		}
	}
	end := len(path)
	if hash := strings.IndexByte(path, '#'); hash >= 0 {
		end = hash
	}
	question := strings.IndexByte(path, '?')
	hasQuery := question >= 0 && question < end
	pathname := path[:end]
	if hasQuery {
		pathname = path[:question]
	}
	for _, segment := range strings.Split(pathname, "/") {
		if segment == "" {
			continue
		}
		if strings.Contains(segment, "%") {
			if decoded, err := decodeURIComponent(segment); err == nil {
				segments = append(segments, decoded)
			} else {
				segments = append(segments, segment)
			}
		} else {
			segments = append(segments, segment)
		}
	}
	if hasQuery && question+1 < end {
		query = parseQueryParams(path[question+1 : end])
	}
	return segments, query
}

// decodeURIComponent decodes a segment the way JavaScript's
// decodeURIComponent does: every '%' must open two hex digits, and the
// result must be valid UTF-8, or the whole decode fails. '+' is literal.
func decodeURIComponent(s string) (string, error) {
	var b []byte
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if i+2 >= len(s) {
				return "", errBadEscape
			}
			hi, ok1 := hexVal(s[i+1])
			lo, ok2 := hexVal(s[i+2])
			if !ok1 || !ok2 {
				return "", errBadEscape
			}
			b = append(b, byte(hi<<4|lo))
			i += 3
			continue
		}
		b = append(b, s[i])
		i++
	}
	if !utf8.Valid(b) {
		return "", errBadEscape
	}
	return string(b), nil
}

var errBadEscape = fmt.Errorf("bad percent-escape")

// ---------------------------------------------------------------------------
// Request heads
// ---------------------------------------------------------------------------

type headerPair struct {
	name, value string
}

// requestHead is a parsed request line plus headers.
type requestHead struct {
	method          string
	target          string
	version         string       // "1.1" | "1.0"
	headers         []headerPair // duplicates joined with ", ", in order
	expectsContinue bool
}

// get returns a header's joined value and whether it was present.
func (h *requestHead) get(name string) (string, bool) {
	for _, p := range h.headers {
		if p.name == name {
			return p.value, true
		}
	}
	return "", false
}

func (h *requestHead) setHeader(name, value string) {
	for i, p := range h.headers {
		if p.name == name {
			h.headers[i].value = p.value + ", " + value
			return
		}
	}
	h.headers = append(h.headers, headerPair{name, value})
}

type headStatus int

const (
	headNeedMore headStatus = iota
	headBad
	headOK
)

// findHeadEnd finds the end of a request head: the blank line, CRLF or LF
// terminated. It returns the offset just past the blank line, or -1.
func findHeadEnd(buf []byte) int {
	crlf := indexOfBytes(buf, []byte("\r\n\r\n"))
	lf := indexOfBytes(buf, []byte("\n\n"))
	if crlf < 0 {
		if lf < 0 {
			return -1
		}
		return lf + 2
	}
	if lf < 0 {
		return crlf + 4
	}
	if lf+2 < crlf+4 {
		return lf + 2
	}
	return crlf + 4
}

func indexOfBytes(b, sep []byte) int {
	for i := 0; i+len(sep) <= len(b); i++ {
		match := true
		for j := range sep {
			if b[i+j] != sep[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// parseHead parses a request head from the pending buffer.
func parseHead(buf []byte) (*requestHead, int, headStatus) {
	end := findHeadEnd(buf)
	if end < 0 {
		if len(buf) > maxHeadBytes {
			return nil, 0, headBad
		}
		return nil, 0, headNeedMore
	}
	if end > maxHeadBytes+4 {
		return nil, 0, headBad
	}
	// The head is decoded byte-for-byte (latin1): header names and values
	// are ASCII in every request the suite sends, and a high byte survives
	// as the matching code point, as Node's ascii decoding does.
	text := latin1String(buf[:end])
	lines := splitHeadLines(text)
	if len(lines) == 0 {
		return nil, 0, headBad
	}
	parts := strings.Split(lines[0], " ")
	if len(parts) != 3 {
		return nil, 0, headBad
	}
	method, target, versionRaw := parts[0], parts[1], parts[2]
	if method == "" || target == "" {
		return nil, 0, headBad
	}
	if versionRaw != "HTTP/1.1" && versionRaw != "HTTP/1.0" {
		return nil, 0, headBad
	}
	head := &requestHead{method: method, target: target, version: versionRaw[len("HTTP/"):]}
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, 0, headBad
		}
		name := strings.ToLower(trimASCIIWS(line[:colon]))
		if name == "" || badHeaderName(name) {
			return nil, 0, headBad
		}
		head.setHeader(name, trimASCIIWS(line[colon+1:]))
	}
	if expect, ok := head.get("expect"); ok {
		for _, token := range strings.Split(strings.ToLower(expect), ",") {
			if trimASCIIWS(token) == "100-continue" {
				head.expectsContinue = true
			}
		}
	}
	return head, end, headOK
}

func latin1String(b []byte) string {
	runes := make([]rune, len(b))
	for i, c := range b {
		runes[i] = rune(c)
	}
	return string(runes)
}

// splitHeadLines splits head text on CRLF or LF line endings.
func splitHeadLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n' {
			lines = append(lines, text[start:i])
			i++
			start = i + 1
		} else if text[i] == '\n' {
			lines = append(lines, text[start:i])
			start = i + 1
		}
	}
	lines = append(lines, text[start:])
	return lines
}

// trimASCIIWS trims ASCII whitespace (space, tab, CR, LF, VT, FF) from both
// ends, matching JavaScript's trim for the ASCII values headers carry.
func trimASCIIWS(s string) string {
	return strings.Trim(s, " \t\r\n\v\f")
}

// badHeaderName reports whether a lowercased, trimmed header name carries a
// character the parser refuses: whitespace, a control, DEL, or the latin1
// non-breaking space (which JavaScript's \s matches).
func badHeaderName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= 0x20 || c == 0x7f || c == 0xa0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Request bodies
// ---------------------------------------------------------------------------

type bodyStatus int

const (
	bodyNeedMore bodyStatus = iota
	bodyBad
	bodyTooLarge
	bodyOK
)

// contentLengthOf parses a content-length from the head: one consistent
// value, or -1 when absent.
func contentLengthOf(head *requestHead) (int64, bodyStatus) {
	raw, ok := head.get("content-length")
	if !ok {
		return -1, bodyOK
	}
	var length int64 = -1
	for _, part := range strings.Split(raw, ",") {
		value := trimASCIIWS(part)
		if !allDigits(value) {
			return 0, bodyBad
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 || parsed > 9007199254740991 {
			return 0, bodyBad
		}
		if length < 0 {
			length = parsed
		} else if length != parsed {
			return 0, bodyBad
		}
	}
	return length, bodyOK
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseBody frames a request body from the bytes after the head.
func parseBody(buf []byte, head *requestHead) ([]byte, int, bodyStatus) {
	if te, ok := head.get("transfer-encoding"); ok {
		for _, token := range strings.Split(strings.ToLower(te), ",") {
			if trimASCIIWS(token) == "chunked" {
				return parseChunkedBody(buf)
			}
		}
	}
	length, st := contentLengthOf(head)
	if st != bodyOK {
		return nil, 0, bodyBad
	}
	if length < 0 {
		length = 0
	}
	if length > maxBodyBytes {
		return nil, 0, bodyTooLarge
	}
	if int64(len(buf)) < length {
		return nil, 0, bodyNeedMore
	}
	return buf[:length], int(length), bodyOK
}

// parseChunkedBody parses a chunked body: size lines, chunks, the zero
// chunk, trailers. An incomplete body reports need-more.
func parseChunkedBody(buf []byte) ([]byte, int, bodyStatus) {
	var chunks [][]byte
	total := 0
	offset := 0
	for {
		eol, eolLen := findLineEnd(buf, offset)
		if eol < 0 {
			return nil, 0, bodyNeedMore
		}
		sizeText := trimASCIIWS(string(buf[offset:eol]))
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = trimASCIIWS(sizeText[:semi])
		}
		if !allHex(sizeText) {
			return nil, 0, bodyBad
		}
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 || size > 9007199254740991 {
			return nil, 0, bodyBad
		}
		offset = eol + eolLen
		if size == 0 {
			// Trailers end at the first blank line; their content is
			// ignored.
			for {
				trailerEol, trailerLen := findLineEnd(buf, offset)
				if trailerEol < 0 {
					return nil, 0, bodyNeedMore
				}
				if trailerEol == offset {
					body := make([]byte, 0, total)
					for _, c := range chunks {
						body = append(body, c...)
					}
					return body, offset + trailerLen, bodyOK
				}
				offset = trailerEol + trailerLen
			}
		}
		total += int(size)
		if total > maxBodyBytes {
			return nil, 0, bodyTooLarge
		}
		if len(buf) < offset+int(size)+2 {
			return nil, 0, bodyNeedMore
		}
		chunks = append(chunks, buf[offset:offset+int(size)])
		offset += int(size)
		if offset+1 < len(buf) && buf[offset] == '\r' && buf[offset+1] == '\n' {
			offset += 2
		} else if buf[offset] == '\n' {
			// A chunk must end CRLF; a lone LF is tolerated the way
			// the head parser tolerates it.
			offset += 1
		} else {
			return nil, 0, bodyBad
		}
	}
}

// findLineEnd finds the next line ending at or after offset: the earliest
// of CRLF and LF, preferring CRLF when both start at the same place. It
// returns the end-of-line offset and the ending's length, or -1.
func findLineEnd(buf []byte, offset int) (int, int) {
	crlf := -1
	lf := -1
	for i := offset; i < len(buf); i++ {
		if buf[i] == '\r' && i+1 < len(buf) && buf[i+1] == '\n' {
			crlf = i
			break
		}
	}
	for i := offset; i < len(buf); i++ {
		if buf[i] == '\n' {
			lf = i
			break
		}
	}
	if crlf >= 0 && (lf < 0 || crlf <= lf) {
		return crlf, 2
	}
	if lf >= 0 {
		return lf, 1
	}
	return -1, 0
}

func allHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// wantsClose reports whether the connection closes after this request's
// reply (HTTP/1.0 default, or `close` asked).
func wantsClose(head *requestHead) bool {
	connection, _ := head.get("connection")
	tokens := strings.Split(strings.ToLower(connection), ",")
	hasClose := false
	hasKeepAlive := false
	for _, token := range tokens {
		switch trimASCIIWS(token) {
		case "close":
			hasClose = true
		case "keep-alive":
			hasKeepAlive = true
		}
	}
	if hasClose {
		return true
	}
	if head.version == "1.0" {
		return !hasKeepAlive
	}
	return false
}

// ---------------------------------------------------------------------------
// Dispatch
// ---------------------------------------------------------------------------

// dispatcher owns the store, the dispatch mutex, and the expected bearer
// token. All handler execution is serialized through the mutex, matching
// the single-threaded reference entry's ordering exactly.
type dispatcher struct {
	store *Store
	mu    sync.Mutex
	token string
}

func (d *dispatcher) authenticated(head *requestHead) bool {
	header, ok := head.get("authorization")
	if !ok || !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := trimASCIIWS(header[len("Bearer "):])
	if token == "" {
		return false
	}
	if d.token == "" {
		return true
	}
	return token == d.token
}

// handle runs one request through dispatch and returns its reply. A handler
// failure becomes the matching JSON error; any other failure (or panic)
// becomes a 500 and the server stays up.
func (d *dispatcher) handle(head *requestHead, body []byte) (out reply) {
	d.mu.Lock()
	defer d.mu.Unlock()
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[forge] request error: %v\n", rec)
			out = jsonErrorReply(d.store, 500, "Internal Server Error")
		}
	}()
	r, err := d.dispatchLocked(head, body)
	if err != nil {
		if se, ok := err.(*StoreError); ok {
			return jsonErrorReply(d.store, se.Status, se.Message)
		}
		fmt.Fprintf(os.Stderr, "[forge] request error: %s\n", err.Error())
		return jsonErrorReply(d.store, 500, "Internal Server Error")
	}
	return r
}

func (d *dispatcher) dispatchLocked(head *requestHead, body []byte) (reply, error) {
	store := d.store
	segments, query := parseRequestTarget(head.target)
	method := strings.ToUpper(head.method)

	// The boot check: no authentication, always up.
	if len(segments) == 1 && segments[0] == "healthz" {
		if method != "GET" {
			return jsonErrorReply(store, 404, "Not Found"), nil
		}
		return reply{
			status: 200,
			headers: []string{
				"content-type: application/json; charset=utf-8",
				"x-github-request-id: " + store.NextRequestID(),
			},
			body: healthzBody,
		}, nil
	}

	// Every other surface requires the bearer token, checked before any
	// body work.
	if !d.authenticated(head) {
		return jsonErrorReply(store, 401, "Bad credentials"), nil
	}

	// MCP: the raw text goes to the handler, which owns the -32700 parse
	// error.
	if len(segments) == 1 && segments[0] == "mcp" {
		if method != "POST" {
			return jsonErrorReply(store, 405, "Method Not Allowed"), nil
		}
		hr, err := handleMcp(store, string(body))
		if err != nil {
			return reply{}, err
		}
		return toReply(hr), nil
	}

	// GraphQL: one JSON object or array per POST.
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if method != "POST" {
			return jsonErrorReply(store, 405, "Method Not Allowed"), nil
		}
		parsed, ok := parseJSONBody(body)
		if !ok {
			return jsonErrorReply(store, 400, "Problems parsing JSON"), nil
		}
		hr, err := handleGraphQL(store, parsed)
		if err != nil {
			return reply{}, err
		}
		return toReply(hr), nil
	}

	// REST v3.
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		parsed, ok := parseJSONBody(body)
		if !ok {
			return jsonErrorReply(store, 400, "Problems parsing JSON"), nil
		}
		host, present := head.get("host")
		if !present {
			host = "localhost"
		}
		hr, err := handleRest(store, restRequest{
			method:   method,
			segments: segments[2:],
			query:    query,
			host:     host,
			body:     parsed,
		})
		if err != nil {
			return reply{}, err
		}
		return toReply(hr), nil
	}

	// Git smart HTTP: /{owner}/{repo}.git/info/refs (GET) and
	// /{owner}/{repo}.git/git-{upload,receive}-pack (POST).
	if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
		owner := segments[0]
		name := segments[1][:len(segments[1])-len(".git")]
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return jsonErrorReply(store, 404, "Not Found"), nil
		}
		if len(segments) == 4 && segments[2] == "info" && segments[3] == "refs" && method == "GET" {
			service := query.get("service")
			gitProtocol, _ := head.get("git-protocol")
			gr := GitInfoRefs(store, owner, name, service, gitProtocol)
			return reply{
				status:  gr.Status,
				headers: []string{"content-type: " + gr.ContentType, "x-github-request-id: " + store.NextRequestID()},
				body:    gr.Body,
			}, nil
		}
		if len(segments) == 3 && method == "POST" && (segments[2] == "git-upload-pack" || segments[2] == "git-receive-pack") {
			var gr GitResponse
			if segments[2] == "git-upload-pack" {
				var err error
				gr, err = GitUploadPack(store, repo, body)
				if err != nil {
					return reply{}, err
				}
			} else {
				gr = GitReceivePack(store, repo, body)
			}
			return reply{
				status:  gr.Status,
				headers: []string{"content-type: " + gr.ContentType, "x-github-request-id: " + store.NextRequestID()},
				body:    gr.Body,
			}, nil
		}
		return jsonErrorReply(store, 404, "Not Found"), nil
	}

	return jsonErrorReply(store, 404, "Not Found"), nil
}

// parseJSONBody parses a request body as JSON. An empty body parses as
// null; a malformed body reports false.
func parseJSONBody(body []byte) (*JVal, bool) {
	if len(body) == 0 {
		return nil, true
	}
	parsed, err := ParseJSON(body)
	if err != nil {
		return nil, false
	}
	return parsed, true
}

// ---------------------------------------------------------------------------
// Connections
// ---------------------------------------------------------------------------

// serveConn runs one connection's request loop: heads and bodies are
// parsed from the pending bytes, each complete request is dispatched, and
// its reply is written before the next request is read, so responses
// always leave in request order.
func serveConn(d *dispatcher, conn net.Conn) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	var pending []byte
	readBuf := make([]byte, 65536)
	var head *requestHead
	for {
		if head == nil {
			parsed, consumed, st := parseHead(pending)
			switch st {
			case headNeedMore:
				n, err := conn.Read(readBuf)
				if err != nil {
					return
				}
				pending = append(pending, readBuf[:n]...)
				continue
			case headBad:
				// An oversized or unparseable head: answer 431 when
				// the head itself is the problem, then close either
				// way.
				if len(pending) > maxHeadBytes {
					conn.Write(serializeReply(jsonErrorReply(d.store, 431, "Request Header Fields Too Large"), true, false))
				}
				return
			case headOK:
				head = parsed
				pending = pending[consumed:]
				if head.expectsContinue {
					if _, err := conn.Write(continue100); err != nil {
						return
					}
				}
			}
		}
		body, consumed, st := parseBody(pending, head)
		switch st {
		case bodyNeedMore:
			n, err := conn.Read(readBuf)
			if err != nil {
				return
			}
			pending = append(pending, readBuf[:n]...)
			continue
		case bodyBad:
			return
		case bodyTooLarge:
			conn.Write(serializeReply(jsonErrorReply(d.store, 500, "Internal Server Error"), true, false))
			return
		case bodyOK:
			pending = pending[consumed:]
			current := head
			head = nil
			reply := d.handle(current, body)
			close := wantsClose(current)
			headOnly := strings.ToUpper(current.method) == "HEAD"
			if _, err := conn.Write(serializeReply(reply, close, headOnly)); err != nil {
				return
			}
			if close {
				return
			}
			// Detach a consumed prefix so a long-lived keep-alive
			// socket never pins a growing buffer.
			if len(pending) == 0 {
				pending = pending[:0]
			} else if cap(pending) > 65536 && len(pending) < cap(pending)/4 {
				compacted := make([]byte, len(pending))
				copy(compacted, pending)
				pending = compacted
			}
		}
	}
}
