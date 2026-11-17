// The one store every surface reads and writes: repositories, git objects
// and refs, issues, comments, labels and pull requests, all in memory. A
// trial is ephemeral and the suite is the only client, so nothing is
// persisted; in-memory maps are the fastest correct store, and every id,
// timestamp and ordering is deterministic (counters and one fixed trial
// clock), so a replayed trial answers exactly as the live one did.
//
// Issues and pull requests share one numbering sequence per repository, as
// on GitHub: a pull request is an item with a head and a base, an issue is
// an item without them.

package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The login the trial's bearer token names: the scenario's viewer, fixed by
// the contest's seed.
const (
	viewerID    = 1000
	viewerLogin = "adaiversen"
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

// The scenario's day one, 2025-01-01T00:00:00Z, the fallback when the trial
// sets no clock.
const fallbackNowSec = 1735689600

// trialClock is the trial's fixed clock: MAKEFUN_TRIAL_NOW when it parses,
// else the scenario's epoch. Sec is whole seconds; ISO is the matching
// JavaScript Date.toISOString() text.
type trialClock struct {
	sec int64
	iso string
}

var digitsOnly = regexp.MustCompile(`^\d+$`)

func loadTrialClock() trialClock {
	raw := trimJSSpace(os.Getenv("MAKEFUN_TRIAL_NOW"))
	if digitsOnly.MatchString(raw) {
		sec, _ := strconv.ParseFloat(raw, 64)
		normalized := sec
		if sec > 1e12 {
			normalized = math.Floor(sec / 1000)
		}
		if iso, ok := jsISODate(normalized); ok {
			return trialClock{sec: int64(normalized), iso: iso}
		}
		// A digit string past JavaScript's Date range crashes the Node
		// entry at startup; staying up on the fallback is the safer
		// behavior for a value no trial sends.
		return trialClock{sec: fallbackNowSec, iso: jsISODateMust(fallbackNowSec)}
	}
	if raw != "" {
		if parsed, ok := parseJSDate(raw); ok {
			sec := int64(math.Floor(float64(parsed) / 1000))
			if iso, ok := jsISODate(float64(sec)); ok {
				return trialClock{sec: sec, iso: iso}
			}
		}
	}
	return trialClock{sec: fallbackNowSec, iso: jsISODateMust(fallbackNowSec)}
}

// jsISODate renders whole epoch seconds as JavaScript's toISOString()
// would: UTC, millisecond precision, always three fraction digits. It
// reports false outside JavaScript's Date range.
func jsISODate(sec float64) (string, bool) {
	ms := sec * 1000
	if ms > 8.64e15 || ms < -8.64e15 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return "", false
	}
	return time.Unix(int64(sec), 0).UTC().Format("2006-01-02T15:04:05.000Z"), true
}

func jsISODateMust(sec int64) string {
	iso, _ := jsISODate(float64(sec))
	return iso
}

// parseJSDate parses the date spellings a trial clock plausibly carries,
// returning epoch milliseconds. JavaScript's Date.parse accepts more
// formats than this; the trial sets epoch seconds or ISO text.
func parseJSDate(raw string) (int64, bool) {
	layouts := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02",
		time.RFC1123Z,
		time.RFC1123,
		time.ANSIC,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// now is the process-wide fixed clock, read once at startup.
var now = loadTrialClock()

// GlobalId renders the opaque GraphQL global ids: prefixed, never the bare
// database id or number.
func globalRepoID(repo *Repo) string    { return fmt.Sprintf("R_%d", repo.ID) }
func globalItemID(item *Item) string    { return fmt.Sprintf("I_%d", item.ID) }
func globalCommentID(c *Comment) string { return fmt.Sprintf("C_%d", c.ID) }

// viewerIdentity is the identity server-created commits carry when the
// request names none.
func viewerIdentity() string { return viewerName + " <" + viewerEmail + ">" }

// Label is a repository label.
type Label struct {
	ID          int
	Name        string
	Color       string
	Description string
}

// Comment is one comment on an item.
type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
}

// ItemKind distinguishes issues from pull requests.
type ItemKind string

const (
	KindIssue ItemKind = "issue"
	KindPull  ItemKind = "pull"
)

// Item is an issue or a pull request. ClosedAt, MergedAt and
// MergeCommitSha are empty when null; HeadRef and BaseRef are empty on an
// issue.
type Item struct {
	ID             int
	Kind           ItemKind
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Labels         []*Label
	Comments       []*Comment
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       string
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
}

// Repo is one repository: its git state plus its issues, labels and pulls.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	FullName      string
	Description   string
	IsPrivate     bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	PushedAt      string
	Objects       map[string]*GitObject
	Refs          map[string]string
	CommitViews   map[string]*CommitView
	TreeCache     map[string][]TreeEntry
	Labels        []*Label
	Items         []*Item
	NextNumber    int
	// Version is bumped on every mutation that can change a cached issue
	// serialization; it lives in every issue-cache key.
	Version int
}

// FileChange is one file's entry in a diff.
type FileChange struct {
	Filename  string
	Status    string // "added" | "modified" | "removed"
	Additions int
	Deletions int
}

// StoreError is a domain failure that maps to an HTTP status and message.
type StoreError struct {
	Status  int
	Message string
}

func (e *StoreError) Error() string { return e.Message }

func newStoreError(status int, format string, args ...any) *StoreError {
	return &StoreError{Status: status, Message: fmt.Sprintf(format, args...)}
}

// byteCache is a bounded string-keyed cache of response bytes. When the map
// grows past its cap it is cleared whole before the next insert.
type byteCache struct {
	m   map[string][]byte
	cap int
}

func newByteCache(cap int) *byteCache { return &byteCache{m: map[string][]byte{}, cap: cap} }

func (c *byteCache) get(key string) ([]byte, bool) {
	v, ok := c.m[key]
	return v, ok
}

func (c *byteCache) put(key string, value []byte) {
	if len(c.m) > c.cap {
		c.m = map[string][]byte{}
	}
	c.m[key] = value
}

func (c *byteCache) invalidatePrefix(prefix string) {
	for key := range c.m {
		if strings.HasPrefix(key, prefix) {
			delete(c.m, key)
		}
	}
}

// Store holds every repository and the registries that join global ids to
// their objects. All access is serialized by the server's dispatch mutex,
// so the maps need no locks of their own.
type Store struct {
	repos       map[string]*Repo
	repoSeq     int
	itemSeq     int
	commentSeq  int
	labelSeq    int
	requestSeq  int
	packCache   *byteCache
	fetchCache  *byteCache
	issueCache  *byteCache
	advCache    *byteCache
	lsRefsCache *byteCache
	repoByID    map[int]*Repo
	itemByID    map[int]*itemLoc
	commentByID map[int]*commentLoc
}

type itemLoc struct {
	repo *Repo
	item *Item
}

type commentLoc struct {
	repo    *Repo
	item    *Item
	comment *Comment
}

// NewStore builds an empty store.
func NewStore() *Store {
	return &Store{
		repos:       map[string]*Repo{},
		repoSeq:     1,
		itemSeq:     1,
		commentSeq:  1,
		labelSeq:    1,
		requestSeq:  1,
		packCache:   newByteCache(64),
		fetchCache:  newByteCache(64),
		issueCache:  newByteCache(1024),
		advCache:    newByteCache(256),
		lsRefsCache: newByteCache(256),
		repoByID:    map[int]*Repo{},
		itemByID:    map[int]*itemLoc{},
		commentByID: map[int]*commentLoc{},
	}
}

// Touch marks a repository changed, invalidating its cached issue
// serializations. The version lives in every issue-cache key, so stale
// entries are unreachable without being deleted.
func (s *Store) Touch(repo *Repo) { repo.Version++ }

// CachedIssueBody returns a cached GET /issues/{n} body, or nil when the
// repository changed since it was built.
func (s *Store) CachedIssueBody(repo *Repo, item *Item) []byte {
	if body, ok := s.issueCache.get(issueCacheKey(repo, item)); ok {
		return body
	}
	return nil
}

func (s *Store) CacheIssueBody(repo *Repo, item *Item, body []byte) {
	s.issueCache.put(issueCacheKey(repo, item), body)
}

func issueCacheKey(repo *Repo, item *Item) string {
	return fmt.Sprintf("%s|issue|%d|%d", repo.FullName, item.Number, repo.Version)
}

// NextRequestID returns a fresh non-empty request id for a reply; a
// counter, so replays agree.
func (s *Store) NextRequestID() string {
	id := fmt.Sprintf("forge-%d", s.requestSeq)
	s.requestSeq++
	return id
}

var (
	globalRepoRe    = regexp.MustCompile(`^R_(\d+)$`)
	globalItemRe    = regexp.MustCompile(`^I_(\d+)$`)
	globalCommentRe = regexp.MustCompile(`^C_(\d+)$`)
)

// RepoByGlobalID returns the repository a global id names, or nil.
func (s *Store) RepoByGlobalID(id string) *Repo {
	m := globalRepoRe.FindStringSubmatch(id)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	return s.repoByID[n]
}

// ItemByGlobalID returns the item a global id names, with its repository.
func (s *Store) ItemByGlobalID(id string) (*Repo, *Item) {
	m := globalItemRe.FindStringSubmatch(id)
	if m == nil {
		return nil, nil
	}
	n, _ := strconv.Atoi(m[1])
	loc := s.itemByID[n]
	if loc == nil {
		return nil, nil
	}
	return loc.repo, loc.item
}

// RepoOfItem returns the repository an item belongs to.
func (s *Store) RepoOfItem(item *Item) *Repo {
	loc := s.itemByID[item.ID]
	if loc == nil {
		return nil
	}
	return loc.repo
}

// NodeKind names what a Relay node id resolved to.
type NodeKind int

const (
	NodeNone NodeKind = iota
	NodeRepo
	NodeItem
	NodeComment
)

// NodeByGlobalID resolves a Relay `node(id:)` global id, whatever its kind.
func (s *Store) NodeByGlobalID(id string) (NodeKind, *Repo, *Item, *Comment) {
	if repo := s.RepoByGlobalID(id); repo != nil {
		return NodeRepo, repo, nil, nil
	}
	if repo, item := s.ItemByGlobalID(id); item != nil {
		return NodeItem, repo, item, nil
	}
	if m := globalCommentRe.FindStringSubmatch(id); m != nil {
		n, _ := strconv.Atoi(m[1])
		if loc := s.commentByID[n]; loc != nil {
			return NodeComment, loc.repo, loc.item, loc.comment
		}
	}
	return NodeNone, nil, nil, nil
}

// ---------------------------------------------------------------------------
// Repositories
// ---------------------------------------------------------------------------

// CreateRepo creates a repository owned by the viewer.
func (s *Store) CreateRepo(name, description string, isPrivate bool) (*Repo, error) {
	key := viewerLogin + "/" + name
	if _, exists := s.repos[key]; exists {
		return nil, newStoreError(422, "Repository %s already exists", name)
	}
	repo := &Repo{
		ID:            s.repoSeq,
		Owner:         viewerLogin,
		Name:          name,
		FullName:      key,
		Description:   description,
		IsPrivate:     isPrivate,
		DefaultBranch: "main",
		CreatedAt:     now.iso,
		UpdatedAt:     now.iso,
		PushedAt:      now.iso,
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		CommitViews:   map[string]*CommitView{},
		TreeCache:     map[string][]TreeEntry{},
		NextNumber:    1,
	}
	s.repoSeq++
	s.repoByID[repo.ID] = repo
	s.repos[key] = repo
	return repo, nil
}

// GetRepo returns the repository named by owner and name, or nil.
func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[owner+"/"+name]
}

// DeleteRepo removes a repository and its registry entries.
func (s *Store) DeleteRepo(repo *Repo) {
	delete(s.repos, repo.FullName)
	delete(s.repoByID, repo.ID)
	for _, item := range repo.Items {
		delete(s.itemByID, item.ID)
		for _, comment := range item.Comments {
			delete(s.commentByID, comment.ID)
		}
	}
	s.InvalidatePackCache(repo)
}

// ListRepos returns every repository, in creation order.
func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, repo := range s.repos {
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------------------------------------------------------------------------
// Git objects and refs
// ---------------------------------------------------------------------------

// PutObject stores an object, returning its id. An object already present
// is kept as-is.
func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte) string {
	oid := HashObject(typ, data)
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

// GetObject returns the object named by oid, or nil.
func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

// PutObjects stores objects, rehashing each, and invalidates the pack cache.
func (s *Store) PutObjects(repo *Repo, objects []*GitObject) {
	for _, object := range objects {
		oid := HashObject(object.Type, object.Data)
		if _, exists := repo.Objects[oid]; !exists {
			repo.Objects[oid] = object
		}
	}
	s.InvalidatePackCache(repo)
}

// PutObjectsMap stores objects already keyed by their ids (a pack reader's
// map): no rehashing, because the reader recomputed every id when it
// resolved the pack. Objects already present are kept.
func (s *Store) PutObjectsMap(repo *Repo, objects map[string]*GitObject) {
	for oid, object := range objects {
		if _, exists := repo.Objects[oid]; !exists {
			repo.Objects[oid] = object
		}
	}
	s.InvalidatePackCache(repo)
}

// GetRef returns the oid a ref names, or "" when the ref does not exist.
func (s *Store) GetRef(repo *Repo, refname string) (string, bool) {
	oid, ok := repo.Refs[refname]
	return oid, ok
}

// SetRef moves a ref, refreshes pushedAt, and invalidates the pack cache.
func (s *Store) SetRef(repo *Repo, refname, oid string) {
	repo.Refs[refname] = oid
	repo.PushedAt = now.iso
	s.InvalidatePackCache(repo)
}

// DeleteRef removes a ref, reporting whether it existed.
func (s *Store) DeleteRef(repo *Repo, refname string) bool {
	if _, ok := repo.Refs[refname]; !ok {
		return false
	}
	delete(repo.Refs, refname)
	s.InvalidatePackCache(repo)
	return true
}

// RefEntry is one ref listing: its name and the oid it names.
type RefEntry struct {
	Name string
	OID  string
}

// ListRefs returns every ref, sorted by name, for a deterministic
// advertisement.
func (s *Store) ListRefs(repo *Repo) []RefEntry {
	out := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveRef resolves a ref spelling to a commit id: a full ref, a short
// branch or tag name, HEAD, or a bare object id (peeled to its commit when
// it names a tag). It reports false when nothing matches, and errors when a
// tag in the chain is malformed.
func (s *Store) ResolveRef(repo *Repo, ref string) (string, bool, error) {
	trimmed := trimJSSpace(ref)
	if trimmed == "" {
		return "", false, nil
	}
	lookup := func(oid string) *GitObject { return repo.Objects[oid] }
	if trimmed == "HEAD" {
		tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return "", false, nil
		}
		peeled, found, err := PeelToCommit(tip, lookup)
		if err != nil {
			return "", false, err
		}
		return peeled, found, nil
	}
	candidates := []string{trimmed, "refs/" + trimmed, "refs/heads/" + trimmed, "refs/tags/" + trimmed}
	for _, candidate := range candidates {
		if oid, ok := repo.Refs[candidate]; ok {
			peeled, found, err := PeelToCommit(oid, lookup)
			if err != nil {
				return "", false, err
			}
			return peeled, found, nil
		}
	}
	if IsObjectID(trimmed) {
		peeled, found, err := PeelToCommit(trimmed, lookup)
		if err != nil {
			return "", false, err
		}
		return peeled, found, nil
	}
	return "", false, nil
}

// ResolveOid resolves a ref spelling to the object id it names, without
// peeling tags.
func (s *Store) ResolveOid(repo *Repo, ref string) (string, bool) {
	trimmed := trimJSSpace(ref)
	if trimmed == "HEAD" {
		oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		return oid, ok
	}
	candidates := []string{trimmed, "refs/" + trimmed, "refs/heads/" + trimmed, "refs/tags/" + trimmed}
	for _, candidate := range candidates {
		if oid, ok := repo.Refs[candidate]; ok {
			return oid, true
		}
	}
	if IsObjectID(trimmed) {
		if _, ok := repo.Objects[trimmed]; ok {
			return trimmed, true
		}
	}
	return "", false
}

// CommitView returns the parsed commit for oid, filled lazily: objects are
// immutable, so a parsed commit never goes stale. A missing object, a
// non-commit, or a malformed commit yields nil.
func (s *Store) CommitView(repo *Repo, oid string) *CommitView {
	if cached, ok := repo.CommitViews[oid]; ok {
		return cached
	}
	object := repo.Objects[oid]
	if object == nil || object.Type != ObjCommit {
		return nil
	}
	view, err := ParseCommit(object.Data)
	if err != nil {
		return nil
	}
	repo.CommitViews[oid] = view
	return view
}

// CommitHistory is the first-parent history from a tip, newest first,
// cycle-safe, capped at limit entries.
func (s *Store) CommitHistory(repo *Repo, tip string, limit int) []string {
	var out []string
	seen := map[string]bool{}
	current, ok := tip, true
	for ok && len(out) < limit && !seen[current] {
		seen[current] = true
		out = append(out, current)
		view := s.CommitView(repo, current)
		if view == nil || len(view.Parents) == 0 {
			ok = false
			continue
		}
		current = view.Parents[0]
	}
	return out
}

// IsAncestor reports whether ancestor is reachable from descendant (or the
// two are equal).
func (s *Store) IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		view := s.CommitView(repo, oid)
		if view == nil {
			continue
		}
		for _, parent := range view.Parents {
			if parent == ancestor {
				return true
			}
			queue = append(queue, parent)
		}
	}
	return false
}

// MergeBase finds a common ancestor of two tips, by breadth-first search
// from the second.
func (s *Store) MergeBase(repo *Repo, a, b string) (string, bool) {
	ancestorsA := map[string]bool{}
	queueA := []string{a}
	for head := 0; head < len(queueA); head++ {
		oid := queueA[head]
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		if view := s.CommitView(repo, oid); view != nil {
			queueA = append(queueA, view.Parents...)
		}
	}
	seen := map[string]bool{}
	queueB := []string{b}
	for head := 0; head < len(queueB); head++ {
		oid := queueB[head]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestorsA[oid] {
			return oid, true
		}
		if view := s.CommitView(repo, oid); view != nil {
			queueB = append(queueB, view.Parents...)
		}
	}
	return "", false
}

// CreateCommit builds and stores a commit carrying the trial clock.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message, author, committer string) (string, error) {
	data, err := BuildCommit(tree, parents, message, author, committer, now.sec, "+0000")
	if err != nil {
		return "", err
	}
	return s.PutObject(repo, ObjCommit, data), nil
}

// ---------------------------------------------------------------------------
// Trees
// ---------------------------------------------------------------------------

// TreeEntries returns a tree's entries, filled lazily. A missing object, a
// non-tree, or a malformed tree yields no entries.
func (s *Store) TreeEntries(repo *Repo, treeOID string) []TreeEntry {
	if cached, ok := repo.TreeCache[treeOID]; ok {
		return cached
	}
	object := repo.Objects[treeOID]
	if object == nil || object.Type != ObjTree {
		return nil
	}
	entries, err := ParseTree(object.Data)
	if err != nil {
		return nil
	}
	repo.TreeCache[treeOID] = entries
	return entries
}

// PathTarget is what readPath found: a blob or a tree, with its mode and
// name.
type PathTarget struct {
	IsTree bool
	OID    string
	Mode   string
	Name   string
}

// ReadPath walks a path from a commit: the blob or tree it names, or nil. A
// trailing slash names the directory itself; an empty path names the root
// tree.
func (s *Store) ReadPath(repo *Repo, commitOID, path string) *PathTarget {
	view := s.CommitView(repo, commitOID)
	if view == nil {
		return nil
	}
	var segments []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	treeOID := view.Tree
	if len(segments) == 0 {
		return &PathTarget{IsTree: true, OID: treeOID, Mode: "40000", Name: ""}
	}
	for i, segment := range segments {
		var entry *TreeEntry
		for _, candidate := range s.TreeEntries(repo, treeOID) {
			if candidate.Name == segment {
				c := candidate
				entry = &c
				break
			}
		}
		if entry == nil {
			return nil
		}
		last := i == len(segments)-1
		if entry.Mode == "40000" {
			if last {
				return &PathTarget{IsTree: true, OID: entry.OID, Mode: entry.Mode, Name: entry.Name}
			}
			treeOID = entry.OID
		} else {
			if !last {
				return nil
			}
			return &PathTarget{IsTree: false, OID: entry.OID, Mode: entry.Mode, Name: entry.Name}
		}
	}
	return nil
}

// FlatFile is one file in a flattened tree: its blob id and mode.
type FlatFile struct {
	OID  string
	Mode string
}

// FlatTree is every file in a commit, path -> blob, with paths sorted.
type FlatTree struct {
	Files map[string]FlatFile
	Paths []string
}

// FlatTreeOf flattens a commit's tree, skipping gitlinks.
func (s *Store) FlatTreeOf(repo *Repo, commitOID string) FlatTree {
	out := FlatTree{Files: map[string]FlatFile{}}
	view := s.CommitView(repo, commitOID)
	if view == nil {
		return out
	}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		for _, entry := range s.TreeEntries(repo, treeOID) {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else if entry.Mode != "160000" {
				out.Files[path] = FlatFile{OID: entry.OID, Mode: entry.Mode}
			}
		}
	}
	walk(view.Tree, "")
	out.Paths = make([]string, 0, len(out.Files))
	for path := range out.Files {
		out.Paths = append(out.Paths, path)
	}
	sort.Strings(out.Paths)
	return out
}

// BuildTreeFromFlat builds (and stores) the nested trees for a flat file
// map, returning the root tree id.
func (s *Store) BuildTreeFromFlat(repo *Repo, files FlatTree) (string, error) {
	type dirNode struct {
		files []TreeEntry
		dirs  map[string]*dirNode
	}
	root := &dirNode{dirs: map[string]*dirNode{}}
	for _, path := range files.Paths {
		file := files.Files[path]
		var segments []string
		for _, seg := range strings.Split(path, "/") {
			if seg != "" {
				segments = append(segments, seg)
			}
		}
		if len(segments) == 0 {
			continue
		}
		node := root
		for i := 0; i < len(segments)-1; i++ {
			child := node.dirs[segments[i]]
			if child == nil {
				child = &dirNode{dirs: map[string]*dirNode{}}
				node.dirs[segments[i]] = child
			}
			node = child
		}
		node.files = append(node.files, TreeEntry{Mode: file.Mode, Name: segments[len(segments)-1], OID: file.OID})
	}
	var build func(node *dirNode) (string, error)
	build = func(node *dirNode) (string, error) {
		entries := make([]TreeEntry, 0, len(node.files)+len(node.dirs))
		entries = append(entries, node.files...)
		dirNames := make([]string, 0, len(node.dirs))
		for name := range node.dirs {
			dirNames = append(dirNames, name)
		}
		sort.Strings(dirNames)
		for _, name := range dirNames {
			oid, err := build(node.dirs[name])
			if err != nil {
				return "", err
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: oid})
		}
		data, err := BuildTree(entries)
		if err != nil {
			return "", err
		}
		return s.PutObject(repo, ObjTree, data), nil
	}
	return build(root)
}

// ---------------------------------------------------------------------------
// Labels, issues, pull requests
// ---------------------------------------------------------------------------

// CreateLabel adds a label to a repository; names are unique per repo.
func (s *Store) CreateLabel(repo *Repo, name, color, description string) (*Label, error) {
	for _, label := range repo.Labels {
		if label.Name == name {
			return nil, newStoreError(422, "Label %s already exists", name)
		}
	}
	label := &Label{ID: s.labelSeq, Name: name, Color: color, Description: description}
	s.labelSeq++
	repo.Labels = append(repo.Labels, label)
	s.Touch(repo)
	return label, nil
}

// GetLabel returns a repository's label by name, or nil.
func (s *Store) GetLabel(repo *Repo, name string) *Label {
	for _, label := range repo.Labels {
		if label.Name == name {
			return label
		}
	}
	return nil
}

// CreateItem opens an issue or pull request, validating that every named
// label exists first (a failure consumes no id).
func (s *Store) CreateItem(repo *Repo, kind ItemKind, title, body string, labelNames []string, headRef, baseRef string) (*Item, error) {
	var labels []*Label
	for _, name := range labelNames {
		label := s.GetLabel(repo, name)
		if label == nil {
			return nil, newStoreError(422, "Label %s does not exist", name)
		}
		labels = append(labels, label)
	}
	item := &Item{
		ID:        s.itemSeq,
		Kind:      kind,
		Number:    repo.NextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    labels,
		CreatedAt: now.iso,
		UpdatedAt: now.iso,
		HeadRef:   headRef,
		BaseRef:   baseRef,
	}
	s.itemSeq++
	repo.NextNumber++
	repo.Items = append(repo.Items, item)
	s.itemByID[item.ID] = &itemLoc{repo: repo, item: item}
	s.Touch(repo)
	return item, nil
}

// GetItem returns a repository's item by number, or nil.
func (s *Store) GetItem(repo *Repo, number int) *Item {
	for _, item := range repo.Items {
		if item.Number == number {
			return item
		}
	}
	return nil
}

// ListItems returns a repository's items in number order.
func (s *Store) ListItems(repo *Repo) []*Item {
	out := make([]*Item, len(repo.Items))
	copy(out, repo.Items)
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

// AddComment appends a comment to an item.
func (s *Store) AddComment(repo *Repo, item *Item, body string) *Comment {
	comment := &Comment{ID: s.commentSeq, Body: body, CreatedAt: now.iso, UpdatedAt: now.iso}
	s.commentSeq++
	item.Comments = append(item.Comments, comment)
	s.commentByID[comment.ID] = &commentLoc{repo: repo, item: item, comment: comment}
	item.UpdatedAt = now.iso
	s.Touch(repo)
	return comment
}

// ---------------------------------------------------------------------------
// Merge and diff
// ---------------------------------------------------------------------------

// MergeResult is the outcome of a three-way merge: the merged file map, or
// the conflicting paths.
type MergeResult struct {
	Merged    FlatTree
	Conflicts []string
	HasMerge  bool
}

// ThreeWayMerge merges head onto base against their merge base. A path
// conflicts when both sides changed it from the merge base to different
// contents.
func (s *Store) ThreeWayMerge(repo *Repo, baseTip, headTip string, mergeBaseOID string, hasBase bool) MergeResult {
	baseFiles := s.FlatTreeOf(repo, baseTip)
	headFiles := s.FlatTreeOf(repo, headTip)
	var ancestorFiles FlatTree
	if hasBase {
		ancestorFiles = s.FlatTreeOf(repo, mergeBaseOID)
	} else {
		ancestorFiles = FlatTree{Files: map[string]FlatFile{}}
	}
	pathSet := map[string]bool{}
	for _, p := range baseFiles.Paths {
		pathSet[p] = true
	}
	for _, p := range headFiles.Paths {
		pathSet[p] = true
	}
	for _, p := range ancestorFiles.Paths {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var conflicts []string
	merged := FlatTree{Files: map[string]FlatFile{}}
	for _, path := range paths {
		base, hasBaseFile := baseFiles.Files[path]
		head, hasHeadFile := headFiles.Files[path]
		ancestor, hasAncestorFile := ancestorFiles.Files[path]
		baseOID, headOID, ancestorOID := "", "", ""
		if hasBaseFile {
			baseOID = base.OID
		}
		if hasHeadFile {
			headOID = head.OID
		}
		if hasAncestorFile {
			ancestorOID = ancestor.OID
		}
		// A missing file and a present file differ exactly when the
		// oids differ; an absent oid reads as "" here, and no real oid
		// is empty, so the comparison is exact.
		baseChanged := (hasBaseFile != hasAncestorFile) || baseOID != ancestorOID
		headChanged := (hasHeadFile != hasAncestorFile) || headOID != ancestorOID
		if baseChanged && headChanged && baseOID != headOID {
			conflicts = append(conflicts, path)
			continue
		}
		if headChanged {
			if hasHeadFile {
				merged.Files[path] = head
			}
		} else if hasBaseFile {
			merged.Files[path] = base
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return MergeResult{Conflicts: conflicts}
	}
	merged.Paths = make([]string, 0, len(merged.Files))
	for p := range merged.Files {
		merged.Paths = append(merged.Paths, p)
	}
	sort.Strings(merged.Paths)
	return MergeResult{Merged: merged, HasMerge: true}
}

// IsMergeable reports whether the pull request would merge cleanly now.
// A malformed tag on the resolve path errors, as the reference entry's
// resolve does.
func (s *Store) IsMergeable(repo *Repo, item *Item) (bool, error) {
	if item.Kind != KindPull || item.State != "open" || item.HeadRef == "" || item.BaseRef == "" {
		return false, nil
	}
	baseTip, ok1, err1 := s.ResolveRef(repo, item.BaseRef)
	headTip, ok2, err2 := s.ResolveRef(repo, item.HeadRef)
	if err1 != nil {
		return false, err1
	}
	if err2 != nil {
		return false, err2
	}
	if !ok1 || !ok2 {
		return false, nil
	}
	if baseTip == headTip {
		return true, nil
	}
	base, hasBase := s.MergeBase(repo, baseTip, headTip)
	return s.ThreeWayMerge(repo, baseTip, headTip, base, hasBase).HasMerge, nil
}

// MergePullRequest squash-merges a pull request: one new commit on the base
// branch carrying the merged tree. It returns the merge commit's sha, or ""
// when the merge conflicts or cannot run (nothing changes).
func (s *Store) MergePullRequest(repo *Repo, item *Item, message string) (string, error) {
	if item.Kind != KindPull || item.State != "open" || item.HeadRef == "" || item.BaseRef == "" {
		return "", nil
	}
	baseTip, ok1, err1 := s.ResolveRef(repo, item.BaseRef)
	headTip, ok2, err2 := s.ResolveRef(repo, item.HeadRef)
	if err1 != nil {
		return "", err1
	}
	if err2 != nil {
		return "", err2
	}
	if !ok1 || !ok2 {
		return "", nil
	}
	close := func(sha string) string {
		item.State = "closed"
		item.Merged = true
		item.MergedAt = now.iso
		item.ClosedAt = now.iso
		item.MergeCommitSha = sha
		item.UpdatedAt = now.iso
		s.Touch(repo)
		return sha
	}
	if baseTip == headTip || s.IsAncestor(repo, headTip, baseTip) {
		return close(baseTip), nil
	}
	base, hasBase := s.MergeBase(repo, baseTip, headTip)
	merged := s.ThreeWayMerge(repo, baseTip, headTip, base, hasBase)
	if !merged.HasMerge {
		return "", nil
	}
	tree, err := s.BuildTreeFromFlat(repo, merged.Merged)
	if err != nil {
		return "", err
	}
	sha, err := s.CreateCommit(repo, tree, []string{baseTip}, message, viewerIdentity(), viewerIdentity())
	if err != nil {
		return "", err
	}
	s.SetRef(repo, "refs/heads/"+item.BaseRef, sha)
	return close(sha), nil
}

// DiffFiles returns the file changes between two commits, with LCS line
// counts. A missing base means an empty tree.
func (s *Store) DiffFiles(repo *Repo, baseOID string, hasBase bool, headOID string) []FileChange {
	var baseFiles FlatTree
	if hasBase {
		baseFiles = s.FlatTreeOf(repo, baseOID)
	} else {
		baseFiles = FlatTree{Files: map[string]FlatFile{}}
	}
	headFiles := s.FlatTreeOf(repo, headOID)
	pathSet := map[string]bool{}
	for _, p := range baseFiles.Paths {
		pathSet[p] = true
	}
	for _, p := range headFiles.Paths {
		pathSet[p] = true
	}
	paths := make([]string, 0, len(pathSet))
	for p := range pathSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var changes []FileChange
	for _, path := range paths {
		base, hasBaseFile := baseFiles.Files[path]
		head, hasHeadFile := headFiles.Files[path]
		if hasBaseFile && hasHeadFile && base.OID == head.OID {
			continue
		}
		baseText := ""
		if hasBaseFile {
			if obj := s.GetObject(repo, base.OID); obj != nil {
				baseText = SanitizeUTF8(string(obj.Data))
			}
		}
		headText := ""
		if hasHeadFile {
			if obj := s.GetObject(repo, head.OID); obj != nil {
				headText = SanitizeUTF8(string(obj.Data))
			}
		}
		switch {
		case !hasBaseFile:
			changes = append(changes, FileChange{Filename: path, Status: "added", Additions: countLines(headText)})
		case !hasHeadFile:
			changes = append(changes, FileChange{Filename: path, Status: "removed", Deletions: countLines(baseText)})
		default:
			additions, deletions := diffLines(baseText, headText)
			changes = append(changes, FileChange{Filename: path, Status: "modified", Additions: additions, Deletions: deletions})
		}
	}
	return changes
}

func countLines(text string) int {
	if text == "" {
		return 0
	}
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		return len(lines) - 1
	}
	return len(lines)
}

// diffLines is the LCS line diff: added and removed line counts.
func diffLines(before, after string) (additions, deletions int) {
	var a, b []string
	if before != "" {
		a = strings.Split(before, "\n")
	}
	if after != "" {
		b = strings.Split(after, "\n")
	}
	if strings.HasSuffix(before, "\n") && len(a) > 0 {
		a = a[:len(a)-1]
	}
	if strings.HasSuffix(after, "\n") && len(b) > 0 {
		b = b[:len(b)-1]
	}
	m, n := len(a), len(b)
	prev := make([]int, n+1)
	curr := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
			} else if prev[j] > curr[j-1] {
				curr[j] = prev[j]
			} else {
				curr[j] = curr[j-1]
			}
		}
		prev, curr = curr, prev
	}
	lcs := prev[n]
	return n - lcs, m - lcs
}

// ---------------------------------------------------------------------------
// Pack caches
// ---------------------------------------------------------------------------

func (s *Store) CachedPack(key string) ([]byte, bool)   { return s.packCache.get(key) }
func (s *Store) CachePack(key string, pack []byte)      { s.packCache.put(key, pack) }
func (s *Store) CachedFetch(key string) ([]byte, bool)  { return s.fetchCache.get(key) }
func (s *Store) CacheFetch(key string, response []byte) { s.fetchCache.put(key, response) }
func (s *Store) CachedAdvertisement(key string) ([]byte, bool) {
	return s.advCache.get(key)
}
func (s *Store) CacheAdvertisement(key string, body []byte) { s.advCache.put(key, body) }
func (s *Store) CachedLsRefs(key string) ([]byte, bool)     { return s.lsRefsCache.get(key) }
func (s *Store) CacheLsRefs(key string, body []byte)        { s.lsRefsCache.put(key, body) }

// InvalidatePackCache drops every cached pack, fetch, advertisement and
// ls-refs entry for one repository. Fetch responses are keyed
// `v2|<cacheKey>` / `v0|<cacheKey>`, so the repository prefix sits after
// the protocol tag; all three shapes must match or stale fetches survive a
// push.
func (s *Store) InvalidatePackCache(repo *Repo) {
	prefix := repo.FullName + "|"
	s.packCache.invalidatePrefix(prefix)
	s.fetchCache.invalidatePrefix(prefix)
	s.fetchCache.invalidatePrefix("v2|" + prefix)
	s.fetchCache.invalidatePrefix("v0|" + prefix)
	s.advCache.invalidatePrefix(prefix)
	s.lsRefsCache.invalidatePrefix(prefix)
}
