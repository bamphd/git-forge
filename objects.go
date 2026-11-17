// Git's object model: hashing, trees, commits and tags, and reachability.
// Every function here is pure and deterministic: the same bytes always hash
// to the same id, and a built object is byte-identical whenever its inputs
// are. No clock, no randomness, no I/O.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ZeroOID is the all-zero object id: the null ref target.
const ZeroOID = "0000000000000000000000000000000000000000"

// ObjType names a git object kind.
type ObjType string

const (
	ObjCommit ObjType = "commit"
	ObjTree   ObjType = "tree"
	ObjBlob   ObjType = "blob"
	ObjTag    ObjType = "tag"
)

// GitObject is one stored object: its kind and its raw bytes.
type GitObject struct {
	Type ObjType
	Data []byte
}

// IsObjectID reports whether s is a lowercase 40-hex object id, the only id
// form the wire accepts.
func IsObjectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// HashObject returns git's object id: SHA-1 over `<type> <size>\0` plus the
// object's bytes.
func HashObject(typ ObjType, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", string(typ), len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// TreeEntry is one entry of a tree object: git's ASCII mode, a name, and an
// object id.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree decodes a tree object into its entries, in stored order.
func ParseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], ' ')
		if space < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		space += offset
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		mode := string(data[offset:space])
		if !isOctal(mode) {
			return nil, fmt.Errorf("tree entry mode %s is not octal", string(AppendJSONString(nil, mode)))
		}
		entries = append(entries, TreeEntry{
			Mode: mode,
			Name: SanitizeUTF8(string(data[space+1 : nul])),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries, nil
}

func isOctal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// compareTreeNames is git's tree-entry sort: byte order of the name, with a
// directory compared as if its name ended in '/'.
func compareTreeNames(a, b TreeEntry) int {
	left := a.Name
	if a.Mode == "40000" {
		left += "/"
	}
	right := b.Name
	if b.Mode == "40000" {
		right += "/"
	}
	return strings.Compare(left, right)
}

// BuildTree encodes entries into a tree object's bytes, sorted the way git
// sorts them.
func BuildTree(entries []TreeEntry) ([]byte, error) {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var out []byte
	for _, e := range sorted {
		if !IsObjectID(e.OID) {
			return nil, fmt.Errorf("tree entry %s has a bad object id", e.Name)
		}
		out = append(out, e.Mode...)
		out = append(out, ' ')
		out = append(out, e.Name...)
		out = append(out, 0)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out, nil
}

// CommitView is a parsed commit: its tree, parents, identities, message,
// and any headers the model does not name.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
	Extra     [][2]string
}

// ParseCommit decodes a commit object; it refuses a malformed header layout.
func ParseCommit(data []byte) (*CommitView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = SanitizeUTF8(string(data[blank+2:]))
	}
	headerText := SanitizeUTF8(string(data[:headerEnd]))
	view := &CommitView{}
	for _, line := range strings.Split(headerText, "\n") {
		if line == "" {
			continue
		}
		key, value := line, ""
		if sp := strings.IndexByte(line, ' '); sp >= 0 {
			key, value = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			view.Tree = value
		case "parent":
			view.Parents = append(view.Parents, value)
		case "author":
			view.Author = value
		case "committer":
			view.Committer = value
		default:
			view.Extra = append(view.Extra, [2]string{key, value})
		}
	}
	if !IsObjectID(view.Tree) {
		return nil, errors.New("commit has no valid tree header")
	}
	for _, parent := range view.Parents {
		if !IsObjectID(parent) {
			return nil, errors.New("commit has a bad parent id")
		}
	}
	view.Message = message
	return view, nil
}

// TagView is a parsed annotated tag.
type TagView struct {
	Object  string
	Type    ObjType
	Tag     string
	Tagger  string
	Message string
}

// ParseTag decodes an annotated tag object.
func ParseTag(data []byte) (*TagView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	message := ""
	if blank >= 0 {
		headerEnd = blank
		message = SanitizeUTF8(string(data[blank+2:]))
	}
	headers := map[string]string{}
	for _, line := range strings.Split(SanitizeUTF8(string(data[:headerEnd])), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return nil, fmt.Errorf("tag header %s has no value", string(AppendJSONString(nil, line)))
		}
		headers[line[:sp]] = line[sp+1:]
	}
	object := headers["object"]
	typ := headers["type"]
	if !IsObjectID(object) {
		return nil, errors.New("tag has no valid object header")
	}
	switch typ {
	case "commit", "tree", "blob", "tag":
	default:
		return nil, fmt.Errorf("tag has bad type %s", string(AppendJSONString(nil, typ)))
	}
	return &TagView{
		Object:  object,
		Type:    ObjType(typ),
		Tag:     headers["tag"],
		Tagger:  headers["tagger"],
		Message: message,
	}, nil
}

// BuildCommit encodes a commit object's bytes: tree, parents, author,
// committer, blank line, message.
func BuildCommit(tree string, parents []string, message, author, committer string, timestamp int64, timezone string) ([]byte, error) {
	if !IsObjectID(tree) {
		return nil, errors.New("buildCommit needs a valid tree id")
	}
	if timezone == "" {
		timezone = "+0000"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", tree)
	for _, parent := range parents {
		if !IsObjectID(parent) {
			return nil, errors.New("buildCommit needs valid parent ids")
		}
		fmt.Fprintf(&b, "parent %s\n", parent)
	}
	fmt.Fprintf(&b, "author %s %d %s\n", author, timestamp, timezone)
	fmt.Fprintf(&b, "committer %s %d %s\n", committer, timestamp, timezone)
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String()), nil
}

// BuildTag encodes an annotated tag object's bytes.
func BuildTag(object string, typ ObjType, tag, message, tagger string, timestamp int64, timezone string) ([]byte, error) {
	if !IsObjectID(object) {
		return nil, errors.New("buildTag needs a valid object id")
	}
	if timezone == "" {
		timezone = "+0000"
	}
	return []byte(fmt.Sprintf("object %s\ntype %s\ntag %s\ntagger %s %d %s\n\n%s",
		object, string(typ), tag, tagger, timestamp, timezone, message)), nil
}

// ObjectLookup resolves an object id to its object, or nil when absent.
type ObjectLookup func(oid string) *GitObject

// CollectReachable returns every object reachable from roots: commit ->
// tree + parents, tree -> entries (gitlinks skipped), tag -> its target,
// blob -> nothing. A missing object is an error unless allowMissing is set;
// a malformed object always errors.
func CollectReachable(roots []string, lookup ObjectLookup, allowMissing bool) (map[string]bool, error) {
	seen := map[string]bool{}
	var queue []string
	for _, root := range roots {
		if IsObjectID(root) {
			queue = append(queue, root)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			if allowMissing {
				continue
			}
			return nil, fmt.Errorf("object %s is missing from the store", oid)
		}
		seen[oid] = true
		switch object.Type {
		case ObjCommit:
			commit, err := ParseCommit(object.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, commit.Tree)
			queue = append(queue, commit.Parents...)
		case ObjTag:
			tag, err := ParseTag(object.Data)
			if err != nil {
				return nil, err
			}
			queue = append(queue, tag.Object)
		case ObjTree:
			entries, err := ParseTree(object.Data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				queue = append(queue, e.OID)
			}
		}
	}
	return seen, nil
}

// PeelToCommit peels a tag (or a chain of tags) to the commit it names, or
// returns the oid itself when it already names a commit. It reports false
// when the oid is missing or names neither a commit nor a tag, and errors
// when a tag in the chain is malformed.
func PeelToCommit(oid string, lookup ObjectLookup) (string, bool, error) {
	current := oid
	for depth := 0; depth < 16; depth++ {
		object := lookup(current)
		if object == nil {
			return "", false, nil
		}
		if object.Type == ObjCommit {
			return current, true, nil
		}
		if object.Type != ObjTag {
			return "", false, nil
		}
		tag, err := ParseTag(object.Data)
		if err != nil {
			return "", false, err
		}
		current = tag.Object
	}
	return "", false, nil
}
