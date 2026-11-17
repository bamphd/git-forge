// Git smart HTTP: the ref advertisements, protocol v2 ls-refs and fetch,
// protocol v0 upload-pack, and receive-pack pushes. Every response is built
// from the store alone; no git binary, no filesystem, no clock. Packs are
// delta-free and cached per request shape, so the fifty concurrent shallow
// clones of the benchmark share one built pack.

package main

import (
	"fmt"
	"sort"
	"strings"
)

// Agent is the user-agent string the forge advertises.
const Agent = "git-forge/1.0"

// GitResponse is one git endpoint's answer: its status, content type, and
// body bytes.
type GitResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitResponse(contentType string, body []byte) GitResponse {
	return GitResponse{Status: 200, ContentType: contentType, Body: body}
}

func gitNotFound() GitResponse {
	return GitResponse{Status: 404, ContentType: "application/json; charset=utf-8", Body: []byte("{\"message\":\"Not Found\"}\n")}
}

// peeledTarget returns the peeled target of an annotated tag ref, or false
// when the ref names no tag.
func peeledTarget(store *Store, repo *Repo, oid string) (string, bool) {
	object := store.GetObject(repo, oid)
	if object == nil || object.Type != ObjTag {
		return "", false
	}
	tag, err := ParseTag(object.Data)
	if err != nil {
		return "", false
	}
	return tag.Object, true
}

// ---------------------------------------------------------------------------
// Advertisements
// ---------------------------------------------------------------------------

// advertisementV0 is the v0 ref advertisement: service preamble, refs with
// capabilities on the first line, flush. Cached per repository and service.
func advertisementV0(store *Store, repo *Repo, service string) []byte {
	cacheKey := repo.FullName + "|adv|" + service
	if cached, ok := store.CachedAdvertisement(cacheKey); ok {
		return cached
	}
	built := buildAdvertisementV0(store, repo, service)
	store.CacheAdvertisement(cacheKey, built)
	return built
}

func buildAdvertisementV0(store *Store, repo *Repo, service string) []byte {
	headRef := "refs/heads/" + repo.DefaultBranch
	var caps string
	if service == "git-upload-pack" {
		caps = "multi_ack thin-pack side-band-64k ofs-delta shallow symref=HEAD:" + headRef + " agent=" + Agent
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=" + Agent
	}
	refs := store.ListRefs(repo)
	var out []byte
	out, _ = AppendPktLine(out, []byte("# service="+service+"\n"))
	out = AppendPktFlush(out)
	if len(refs) == 0 {
		out, _ = AppendPktLine(out, []byte(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		out = AppendPktFlush(out)
		return out
	}
	var lines []string
	if headTip, ok := store.GetRef(repo, headRef); ok {
		lines = append(lines, headTip+" HEAD\n")
	}
	for _, ref := range refs {
		lines = append(lines, ref.OID+" "+ref.Name+"\n")
		if peeled, ok := peeledTarget(store, repo, ref.OID); ok {
			lines = append(lines, peeled+" "+ref.Name+"^{}\n")
		}
	}
	first := lines[0]
	lines[0] = first[:len(first)-1] + "\x00" + caps + "\n"
	for _, line := range lines {
		out, _ = AppendPktLine(out, []byte(line))
	}
	out = AppendPktFlush(out)
	return out
}

// advertisementV2 is the v2 capability advertisement: `version 2`,
// capability lines, flush. No service header. It never varies, so one copy
// serves every response.
var advertisementV2 = func() []byte {
	var out []byte
	for _, line := range []string{"version 2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n", "agent=" + Agent + "\n"} {
		out, _ = AppendPktLine(out, []byte(line))
	}
	return AppendPktFlush(out)
}()

// GitInfoRefs answers GET /info/refs. Upload-pack answers v2 when the
// client asks with Git-Protocol: version=2 and v0 otherwise; receive-pack
// always answers v0. A missing repository is 404 and the server stays up.
func GitInfoRefs(store *Store, owner, name, service, gitProtocolHeader string) GitResponse {
	repo := store.GetRepo(owner, name)
	if repo == nil {
		return gitNotFound()
	}
	var body []byte
	if service == "git-upload-pack" && gitProtocolHeader == "version=2" {
		body = advertisementV2
	} else {
		body = advertisementV0(store, repo, service)
	}
	return gitResponse("application/x-"+service+"-advertisement", body)
}

// ---------------------------------------------------------------------------
// Side-band
// ---------------------------------------------------------------------------

// sidebandWrap wraps bytes in side-band-64k packets (band byte + chunk,
// pkt-lined), plus a trailing flush.
func sidebandWrap(data []byte, band byte) []byte {
	const chunk = 65515
	packets := 1
	if len(data) > 0 {
		packets = (len(data) + chunk - 1) / chunk
	}
	out := make([]byte, 0, packets*5+len(data)+4)
	writePacket := func(part []byte) {
		out = append(out, pktLenHex(len(part)+5)...)
		out = append(out, band)
		out = append(out, part...)
	}
	if len(data) == 0 {
		writePacket(nil)
	} else {
		for offset := 0; offset < len(data); offset += chunk {
			end := offset + chunk
			if end > len(data) {
				end = len(data)
			}
			writePacket(data[offset:end])
		}
	}
	return AppendPktFlush(out)
}

// ---------------------------------------------------------------------------
// Fetch planning
// ---------------------------------------------------------------------------

// FetchPlan is a planned fetch: objects to pack (sorted by oid for
// deterministic bytes), boundary commits whose parents were cut off by the
// depth limit, client shallow boundaries this fetch completes, and haves
// that are ancestors of a want.
type FetchPlan struct {
	Objects   []*GitObject
	Shallow   []string
	Unshallow []string
	Acks      []string
}

// generations computes commit generations from the wants (want = 1, parents
// increment), following all parents, first visit winning.
func generations(store *Store, repo *Repo, wants []string) (map[string]int, error) {
	lookup := func(oid string) *GitObject { return store.GetObject(repo, oid) }
	gen := map[string]int{}
	var queue []string
	for _, want := range wants {
		peeled, found, err := PeelToCommit(want, lookup)
		if err != nil {
			return nil, err
		}
		if found {
			if _, ok := gen[peeled]; !ok {
				gen[peeled] = 1
				queue = append(queue, peeled)
			}
		}
	}
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		depth := gen[oid]
		view := store.CommitView(repo, oid)
		if view == nil {
			continue
		}
		for _, parent := range view.Parents {
			if _, ok := gen[parent]; !ok {
				gen[parent] = depth + 1
				queue = append(queue, parent)
			}
		}
	}
	return gen, nil
}

// PlanFetch plans a fetch: which objects to send, which commits are
// shallow, which client boundaries are completed, and which haves to ACK.
// Depth limits commit generations only; a commit's trees and blobs always
// travel whole. Haves exclude their reachable sets, stopping at the
// client's own shallow boundaries, which the client does not hold beyond.
func PlanFetch(store *Store, repo *Repo, wants, haves []string, depth *int, clientShallow []string) (*FetchPlan, error) {
	lookup := func(oid string) *GitObject { return store.GetObject(repo, oid) }
	gen, err := generations(store, repo, wants)
	if err != nil {
		return nil, err
	}
	includedCommits := map[string]bool{}
	for oid, depthOf := range gen {
		if depth == nil || depthOf <= *depth {
			includedCommits[oid] = true
		}
	}
	// The wanted set: included commits, plus tags naming them (a wanted tag
	// travels with its target).
	roots := map[string]bool{}
	for oid := range includedCommits {
		roots[oid] = true
	}
	for _, want := range wants {
		if object := lookup(want); object != nil && object.Type == ObjTag {
			roots[want] = true
		}
	}
	// Walk from the roots, following trees fully but parent links only into
	// included commits: a depth limit must cut history off, not merely
	// filter the starting set while the walk re-adds every ancestor.
	reachable := map[string]bool{}
	{
		var stack []string
		for oid := range roots {
			stack = append(stack, oid)
		}
		seen := map[string]bool{}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			object := lookup(oid)
			if object == nil {
				continue
			}
			reachable[oid] = true
			switch object.Type {
			case ObjCommit:
				view := store.CommitView(repo, oid)
				if view == nil {
					continue
				}
				stack = append(stack, view.Tree)
				for _, parent := range view.Parents {
					if includedCommits[parent] {
						stack = append(stack, parent)
					}
				}
			case ObjTag:
				tag, err := ParseTag(object.Data)
				if err != nil {
					continue // A corrupt tag contributes nothing further.
				}
				targetObject := lookup(tag.Object)
				if targetObject != nil && targetObject.Type == ObjCommit {
					if includedCommits[tag.Object] {
						stack = append(stack, tag.Object)
					}
				} else {
					stack = append(stack, tag.Object)
				}
			case ObjTree:
				for _, entry := range store.TreeEntries(repo, oid) {
					if entry.Mode != "160000" {
						stack = append(stack, entry.OID)
					}
				}
			}
		}
	}
	// Shallow: boundary commits with parents in the store, whose parents
	// were cut off.
	var shallow []string
	if depth != nil {
		for oid, depthOf := range gen {
			if depthOf != *depth {
				continue
			}
			view := store.CommitView(repo, oid)
			if view != nil && len(view.Parents) > 0 {
				shallow = append(shallow, oid)
			}
		}
	}
	sort.Strings(shallow)
	// Haves exclude their reachable sets, stopping traversal at client
	// shallow boundaries: the client holds a boundary commit and its tree,
	// but not the boundary's parents.
	excluded := map[string]bool{}
	boundarySet := map[string]bool{}
	for _, b := range clientShallow {
		boundarySet[b] = true
	}
	for _, have := range haves {
		queue := []string{have}
		seen := map[string]bool{}
		for len(queue) > 0 {
			oid := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			object := lookup(oid)
			if object == nil {
				continue
			}
			excluded[oid] = true
			switch object.Type {
			case ObjCommit:
				if boundarySet[oid] {
					// Include the commit's tree, not its parents.
					if view := store.CommitView(repo, oid); view != nil {
						treeReachable, err := CollectReachable([]string{view.Tree}, lookup, true)
						if err != nil {
							return nil, err
						}
						for treeOID := range treeReachable {
							excluded[treeOID] = true
						}
					}
					continue
				}
				if view := store.CommitView(repo, oid); view != nil {
					queue = append(queue, view.Tree)
					queue = append(queue, view.Parents...)
				}
			case ObjTag:
				tag, err := ParseTag(object.Data)
				if err != nil {
					continue // A corrupt tag contributes nothing further.
				}
				queue = append(queue, tag.Object)
			case ObjTree:
				for _, entry := range store.TreeEntries(repo, oid) {
					if entry.Mode != "160000" {
						queue = append(queue, entry.OID)
					}
				}
			}
		}
	}
	var objects []*GitObject
	sortedReachable := make([]string, 0, len(reachable))
	for oid := range reachable {
		sortedReachable = append(sortedReachable, oid)
	}
	sort.Strings(sortedReachable)
	for _, oid := range sortedReachable {
		if excluded[oid] {
			continue
		}
		if object := lookup(oid); object != nil {
			objects = append(objects, object)
		}
	}
	// Unshallow: client boundaries reachable from the wants whose parents
	// are all included.
	var unshallow []string
	for _, boundary := range clientShallow {
		if !reachable[boundary] {
			continue
		}
		view := store.CommitView(repo, boundary)
		if view == nil {
			continue
		}
		all := true
		for _, parent := range view.Parents {
			if !reachable[parent] || !includedCommits[parent] {
				all = false
				break
			}
		}
		if all {
			unshallow = append(unshallow, boundary)
		}
	}
	sort.Strings(unshallow)
	// ACKs: haves that are ancestors of a want, in request order,
	// deduplicated.
	var acks []string
	seenAck := map[string]bool{}
	for _, have := range haves {
		if seenAck[have] {
			continue
		}
		seenAck[have] = true
		for _, want := range wants {
			peeled, found, err := PeelToCommit(want, lookup)
			if err != nil {
				return nil, err
			}
			if found && store.IsAncestor(repo, have, peeled) {
				acks = append(acks, have)
				break
			}
		}
	}
	return &FetchPlan{Objects: objects, Shallow: shallow, Unshallow: unshallow, Acks: acks}, nil
}

// fetchPack returns the pack for a fetch plan, cached by the request shape
// so repeated clones share one build.
func fetchPack(store *Store, repo *Repo, plan *FetchPlan, cacheKey string) []byte {
	if cached, ok := store.CachedPack(cacheKey); ok {
		return cached
	}
	pack := WritePack(plan.Objects)
	store.CachePack(cacheKey, pack)
	return pack
}

func fetchCacheKey(repo *Repo, wants, haves []string, depth *int, clientShallow []string) string {
	sorted := func(in []string) string {
		cp := make([]string, len(in))
		copy(cp, in)
		sort.Strings(cp)
		return strings.Join(cp, ",")
	}
	depthText := ""
	if depth != nil {
		depthText = fmt.Sprintf("%d", *depth)
	}
	return repo.FullName + "|fetch|" + sorted(wants) + "|" + sorted(haves) + "|" + depthText + "|" + sorted(clientShallow)
}

// ---------------------------------------------------------------------------
// v2 commands
// ---------------------------------------------------------------------------

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func handleLsRefs(store *Store, repo *Repo, args lsRefsArgs) []byte {
	prefixJSON := MarshalJSON(prefixArrayJSON(args.prefixes))
	cacheKey := fmt.Sprintf("%s|lsrefs|%d%d%d|%s", repo.FullName, boolBit(args.peel), boolBit(args.symrefs), boolBit(args.unborn), prefixJSON)
	if cached, ok := store.CachedLsRefs(cacheKey); ok {
		return cached
	}
	built := buildLsRefs(store, repo, args)
	store.CacheLsRefs(cacheKey, built)
	return built
}

func boolBit(b bool) int {
	if b {
		return 1
	}
	return 0
}

func prefixArrayJSON(prefixes []string) *JVal {
	arr := JArr()
	for _, p := range prefixes {
		arr.Arr = append(arr.Arr, JStr(p))
	}
	return arr
}

func buildLsRefs(store *Store, repo *Repo, args lsRefsArgs) []byte {
	headRef := "refs/heads/" + repo.DefaultBranch
	refs := store.ListRefs(repo)
	var lines []string
	keep := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, prefix := range args.prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	if keep("HEAD") {
		if headTip, ok := store.GetRef(repo, headRef); !ok {
			if args.unborn {
				if args.symrefs {
					lines = append(lines, "unborn HEAD symref-target:"+headRef+"\n")
				} else {
					lines = append(lines, "unborn HEAD\n")
				}
			}
		} else {
			if args.symrefs {
				lines = append(lines, headTip+" HEAD symref-target:"+headRef+"\n")
			} else {
				lines = append(lines, headTip+" HEAD\n")
			}
		}
	}
	for _, ref := range refs {
		if !keep(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if args.peel {
			if peeled, ok := peeledTarget(store, repo, ref.OID); ok {
				line += " peeled:" + peeled
			}
		}
		lines = append(lines, line+"\n")
	}
	var out []byte
	for _, line := range lines {
		out, _ = AppendPktLine(out, []byte(line))
	}
	return AppendPktFlush(out)
}

func handleFetchV2(store *Store, repo *Repo, wants, haves []string, depth *int, clientShallow []string) ([]byte, error) {
	cacheKey := fetchCacheKey(repo, wants, haves, depth, clientShallow)
	responseKey := "v2|" + cacheKey
	if cached, ok := store.CachedFetch(responseKey); ok {
		return cached, nil
	}
	plan, err := PlanFetch(store, repo, wants, haves, depth, clientShallow)
	if err != nil {
		return nil, err
	}
	pack := fetchPack(store, repo, plan, cacheKey)
	var out []byte
	// Acknowledgments: only when the client sent haves. A client that sent
	// none (a fresh clone) expects the packfile section next and rejects an
	// acknowledgments section outright, as real git does; when haves were
	// sent, ACKs plus ready, or NAK alone.
	if len(haves) > 0 {
		out, _ = AppendPktLine(out, []byte("acknowledgments\n"))
		if len(plan.Acks) > 0 {
			for _, ack := range plan.Acks {
				out, _ = AppendPktLine(out, []byte("ACK "+ack+"\n"))
			}
			out, _ = AppendPktLine(out, []byte("ready\n"))
		} else {
			out, _ = AppendPktLine(out, []byte("NAK\n"))
		}
		out = AppendPktDelim(out)
	}
	// Shallow info, only when there is something to say.
	if len(plan.Shallow) > 0 || len(plan.Unshallow) > 0 {
		out, _ = AppendPktLine(out, []byte("shallow-info\n"))
		for _, oid := range plan.Shallow {
			out, _ = AppendPktLine(out, []byte("shallow "+oid+"\n"))
		}
		for _, oid := range plan.Unshallow {
			out, _ = AppendPktLine(out, []byte("unshallow "+oid+"\n"))
		}
		out = AppendPktDelim(out)
	}
	// Packfile: header, then the pack in side-band packets, then flush.
	out, _ = AppendPktLine(out, []byte("packfile\n"))
	out = append(out, sidebandWrap(pack, 1)...)
	store.CacheFetch(responseKey, out)
	return out, nil
}

func handleFetchV0(store *Store, repo *Repo, wants, haves []string, depth *int, clientShallow []string) ([]byte, error) {
	cacheKey := fetchCacheKey(repo, wants, haves, depth, clientShallow)
	responseKey := "v0|" + cacheKey
	if cached, ok := store.CachedFetch(responseKey); ok {
		return cached, nil
	}
	plan, err := PlanFetch(store, repo, wants, haves, depth, clientShallow)
	if err != nil {
		return nil, err
	}
	pack := fetchPack(store, repo, plan, cacheKey)
	var headLines []string
	if len(plan.Acks) > 0 {
		for _, ack := range plan.Acks {
			headLines = append(headLines, "ACK "+ack+"\n")
		}
	} else {
		headLines = append(headLines, "NAK\n")
	}
	for _, oid := range plan.Shallow {
		headLines = append(headLines, "shallow "+oid+"\n")
	}
	for _, oid := range plan.Unshallow {
		headLines = append(headLines, "unshallow "+oid+"\n")
	}
	var out []byte
	if len(headLines) > 0 {
		for _, line := range headLines {
			out, _ = AppendPktLine(out, []byte(line))
		}
	}
	out = append(out, sidebandWrap(pack, 1)...)
	store.CacheFetch(responseKey, out)
	return out, nil
}

// parseDeepen parses a deepen argument the way JavaScript's parseInt with
// radix 10 does: a leading optional sign and digit run, trailing garbage
// ignored, and the result must be a safe integer above zero.
func parseDeepen(text string) (int, bool) {
	s := trimJSSpace(text)
	i := 0
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var acc float64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		acc = acc*10 + float64(s[i]-'0')
		i++
	}
	if i == start {
		return 0, false
	}
	if neg {
		acc = -acc
	}
	if acc <= 0 || acc > 9007199254740991 {
		return 0, false
	}
	return int(acc), true
}

func stripOneNewline(line string) string {
	return strings.TrimSuffix(line, "\n")
}

// GitUploadPack answers POST /git-upload-pack. A body starting with
// `command=` is protocol v2 (ls-refs or fetch); anything else is v0
// (want/have/done). Unknown v2 commands answer a side-band error the client
// surfaces, never a crash.
func GitUploadPack(store *Store, repo *Repo, body []byte) (GitResponse, error) {
	packets, _, err := DecodePktLines(body, false)
	if err != nil {
		return GitResponse{}, err
	}
	lines := PacketsToLines(packets)
	first := ""
	if len(lines) > 0 {
		first = trimJSSpace(lines[0])
	}
	if strings.HasPrefix(first, "command=") {
		command := first[len("command="):]
		// Arguments live after the delim packet; everything before it is
		// the command envelope.
		delimIndex := -1
		for i, p := range packets {
			if p.Kind == PktDelim {
				delimIndex = i
				break
			}
		}
		var argPackets []Packet
		if delimIndex >= 0 {
			argPackets = packets[delimIndex+1:]
		} else {
			argPackets = packets[1:]
		}
		var args []string
		for _, line := range PacketsToLines(argPackets) {
			args = append(args, stripOneNewline(line))
		}
		if command == "ls-refs" {
			lsArgs := lsRefsArgs{}
			for _, arg := range args {
				switch {
				case arg == "peel":
					lsArgs.peel = true
				case arg == "symrefs":
					lsArgs.symrefs = true
				case arg == "unborn":
					lsArgs.unborn = true
				case strings.HasPrefix(arg, "ref-prefix "):
					lsArgs.prefixes = append(lsArgs.prefixes, arg[len("ref-prefix "):])
				}
			}
			return gitResponse("application/x-git-upload-pack-result", handleLsRefs(store, repo, lsArgs)), nil
		}
		if command == "fetch" {
			var wants, haves, clientShallow []string
			var depth *int
			for _, arg := range args {
				switch {
				case strings.HasPrefix(arg, "want "):
					wants = append(wants, trimJSSpace(arg[5:]))
				case strings.HasPrefix(arg, "have "):
					haves = append(haves, trimJSSpace(arg[5:]))
				case strings.HasPrefix(arg, "shallow "):
					clientShallow = append(clientShallow, trimJSSpace(arg[8:]))
				case strings.HasPrefix(arg, "deepen "):
					if d, ok := parseDeepen(arg[7:]); ok {
						depth = &d
					}
				}
			}
			response, err := handleFetchV2(store, repo, wants, haves, depth, clientShallow)
			if err != nil {
				return GitResponse{}, err
			}
			return gitResponse("application/x-git-upload-pack-result", response), nil
		}
		// Unknown command: a packfile section carrying a side-band error,
		// which the client reports.
		var out []byte
		out, _ = AppendPktLine(out, []byte("packfile\n"))
		errPacket := append([]byte{3}, []byte("unknown command "+command+"\n")...)
		out, _ = AppendPktLine(out, errPacket)
		out = AppendPktFlush(out)
		return gitResponse("application/x-git-upload-pack-result", out), nil
	}
	// Protocol v0: want/have/shallow/deepen/done lines, in one or two
	// flush-separated sections.
	var wants, haves, clientShallow []string
	var depth *int
	for _, raw := range lines {
		line := stripOneNewline(raw)
		switch {
		case strings.HasPrefix(line, "want "):
			oid := strings.Split(line[5:], " ")[0]
			if IsObjectID(oid) {
				wants = append(wants, oid)
			}
		case strings.HasPrefix(line, "have "):
			oid := trimJSSpace(line[5:])
			if IsObjectID(oid) {
				haves = append(haves, oid)
			}
		case strings.HasPrefix(line, "shallow "):
			oid := trimJSSpace(line[8:])
			if IsObjectID(oid) {
				clientShallow = append(clientShallow, oid)
			}
		case strings.HasPrefix(line, "deepen "):
			if d, ok := parseDeepen(line[7:]); ok {
				depth = &d
			}
		}
	}
	response, err := handleFetchV0(store, repo, wants, haves, depth, clientShallow)
	if err != nil {
		return GitResponse{}, err
	}
	return gitResponse("application/x-git-upload-pack-result", response), nil
}

// ---------------------------------------------------------------------------
// Receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// GitReceivePack answers POST /git-receive-pack. It parses the command list
// and the pack that follows its flush, stores the pack's objects, then
// applies each ref update independently: fast-forwards move, anything else
// is refused per ref with its reason. A corrupt pack refuses every ref with
// an unpack error. The report is always well-formed; a malformed body never
// crashes the server.
func GitReceivePack(store *Store, repo *Repo, body []byte) GitResponse {
	var commands []pushCommand
	var packBytes []byte
	sideband := true
	parseError := ""
	packets, rest, err := DecodePktLines(body, false)
	if err != nil {
		parseError = err.Error()
	} else {
		packBytes = rest
		lines := PacketsToLines(packets)
		for index, raw := range lines {
			line := stripOneNewline(raw)
			if index == 0 {
				if nul := strings.IndexByte(line, 0); nul >= 0 {
					caps := strings.Split(line[nul+1:], " ")
					sideband = false
					for _, cap := range caps {
						if cap == "side-band-64k" {
							sideband = true
							break
						}
					}
					line = line[:nul]
				}
			}
			if line == "" {
				continue
			}
			parts := strings.Split(line, " ")
			if len(parts) != 3 || !IsObjectID(parts[0]) || !IsObjectID(parts[1]) {
				parseError = "bad command line " + string(AppendJSONString(nil, firstRunes(line, 60)))
				break
			}
			commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}

	var reportLines []string
	unpackOK := false
	unpackDetail := "ok"
	if parseError != "" {
		unpackDetail = parseError
	} else if len(commands) == 0 {
		unpackDetail = "no commands"
	} else {
		updatesRef := false
		for _, command := range commands {
			if command.newOID != ZeroOID {
				updatesRef = true
				break
			}
		}
		var packObjects map[string]*GitObject
		packRead := false
		if updatesRef || len(packBytes) > 0 {
			if len(packBytes) == 0 {
				unpackDetail = "missing packfile"
			} else {
				read, err := ReadPack(packBytes, func(oid string) *GitObject { return store.GetObject(repo, oid) })
				if err != nil {
					unpackDetail = err.Error()
				} else if !read.ChecksumOK {
					unpackDetail = "pack checksum mismatch"
				} else {
					packObjects = read.Objects
					packRead = true
				}
			}
		} else {
			packObjects = map[string]*GitObject{}
			packRead = true
		}
		if packRead {
			unpackOK = true
			store.PutObjectsMap(repo, packObjects)
			for _, command := range commands {
				current, exists := store.GetRef(repo, command.ref)
				if command.newOID == ZeroOID {
					// Delete.
					if !exists {
						reportLines = append(reportLines, "ng "+command.ref+" remote ref does not exist\n")
					} else if command.oldOID != ZeroOID && command.oldOID != current {
						reportLines = append(reportLines, "ng "+command.ref+" stale info\n")
					} else {
						store.DeleteRef(repo, command.ref)
						reportLines = append(reportLines, "ok "+command.ref+"\n")
					}
					continue
				}
				if store.GetObject(repo, command.newOID) == nil {
					reportLines = append(reportLines, "ng "+command.ref+" object not found\n")
					continue
				}
				if command.oldOID == ZeroOID {
					// Create.
					if exists {
						reportLines = append(reportLines, "ng "+command.ref+" already exists\n")
					} else {
						store.SetRef(repo, command.ref, command.newOID)
						reportLines = append(reportLines, "ok "+command.ref+"\n")
					}
					continue
				}
				// Update.
				if !exists {
					reportLines = append(reportLines, "ng "+command.ref+" remote ref does not exist\n")
				} else if current != command.oldOID {
					reportLines = append(reportLines, "ng "+command.ref+" stale info\n")
				} else if current == command.newOID {
					reportLines = append(reportLines, "ok "+command.ref+"\n")
				} else if !store.IsAncestor(repo, current, command.newOID) {
					reportLines = append(reportLines, "ng "+command.ref+" non-fast-forward\n")
				} else {
					store.SetRef(repo, command.ref, command.newOID)
					reportLines = append(reportLines, "ok "+command.ref+"\n")
				}
			}
		} else {
			for _, command := range commands {
				reportLines = append(reportLines, "ng "+command.ref+" unpacker error\n")
			}
		}
	}
	if !unpackOK && len(reportLines) == 0 {
		for _, command := range commands {
			reportLines = append(reportLines, "ng "+command.ref+" unpacker error\n")
		}
	}
	unpackLine := "unpack ok\n"
	if !unpackOK {
		unpackLine = "unpack " + unpackDetail + "\n"
	}
	var report []byte
	report, _ = AppendPktLine(report, []byte(unpackLine))
	for _, line := range reportLines {
		report, _ = AppendPktLine(report, []byte(line))
	}
	report = AppendPktFlush(report)
	framed := report
	if sideband {
		framed = sidebandWrap(report, 1)
	}
	return gitResponse("application/x-git-receive-pack-result", framed)
}

// firstRunes returns the first n runes of s (JavaScript's String slice
// counts UTF-16 code units; for the ASCII command lines this meets, rune
// counting agrees).
func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
