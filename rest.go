// The REST v3 surface: repositories, branches, commits, contents, labels,
// issues, comments and pull requests. Every reply is application/json with
// a non-empty X-GitHub-Request-Id, every error carries a string `message`,
// and unknown routes are JSON 404s. All state lives in the one store; this
// module only translates HTTP into store calls and store objects into the
// GitHub shapes the suite asserts.

package main

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const jsonType = "application/json; charset=utf-8"

// restRequest is what the REST handler receives: the uppercased method,
// the path segments after /api/v3, the parsed query, the Host header, and
// the parsed JSON body (nil for an empty body or a JSON null).
type restRequest struct {
	method   string
	segments []string
	query    queryParams
	host     string
	body     *JVal
}

// jsonReply serializes one JSON value with a trailing newline, under the
// standard headers plus any extras (a Link header, an OAuth-scopes
// header), in that order.
func jsonReply(store *Store, status int, value *JVal, extras ...[2]string) handlerReply {
	body := AppendJSON(nil, value)
	body = append(body, '\n')
	headers := [][2]string{
		{"content-type", jsonType},
		{"x-github-request-id", store.NextRequestID()},
	}
	headers = append(headers, extras...)
	return handlerReply{status: status, headers: headers, body: body}
}

func restErrorReply(store *Store, status int, message string) handlerReply {
	return jsonReply(store, status, JObj().Set("message", JStr(message)))
}

func restNotFound(store *Store) handlerReply {
	return restErrorReply(store, 404, "Not Found")
}

// ---------------------------------------------------------------------------
// Serializers
// ---------------------------------------------------------------------------

func userObject() *JVal {
	return JObj().
		Set("login", JStr(viewerLogin)).
		Set("id", JInt(viewerID)).
		Set("name", JStr(viewerName)).
		Set("email", JStr(viewerEmail)).
		Set("type", JStr("User")).
		Set("site_admin", JBool(false)).
		Set("created_at", JStr(now.iso)).
		Set("updated_at", JStr(now.iso))
}

func labelObject(label *Label) *JVal {
	return JObj().
		Set("id", JInt(int64(label.ID))).
		Set("name", JStr(label.Name)).
		Set("color", JStr(label.Color)).
		Set("description", JStr(label.Description)).
		Set("default", JBool(false))
}

func repoObject(store *Store, repo *Repo) *JVal {
	openIssues := 0
	for _, item := range repo.Items {
		if item.State == "open" {
			openIssues++
		}
	}
	visibility := "public"
	if repo.IsPrivate {
		visibility = "private"
	}
	return JObj().
		Set("id", JInt(int64(repo.ID))).
		Set("name", JStr(repo.Name)).
		Set("full_name", JStr(repo.FullName)).
		Set("owner", userObject()).
		Set("private", JBool(repo.IsPrivate)).
		Set("description", JStr(repo.Description)).
		Set("default_branch", JStr(repo.DefaultBranch)).
		Set("created_at", JStr(repo.CreatedAt)).
		Set("updated_at", JStr(repo.UpdatedAt)).
		Set("pushed_at", JStr(repo.PushedAt)).
		Set("has_issues", JBool(true)).
		Set("has_projects", JBool(true)).
		Set("has_wiki", JBool(true)).
		Set("has_pages", JBool(false)).
		Set("has_downloads", JBool(true)).
		Set("archived", JBool(false)).
		Set("disabled", JBool(false)).
		Set("visibility", JStr(visibility)).
		Set("forks_count", JInt(0)).
		Set("open_issues_count", JInt(int64(openIssues))).
		Set("watchers_count", JInt(0)).
		Set("size", JInt(0)).
		Set("language", JNul()).
		Set("topics", JArr())
}

func commentObject(comment *Comment) *JVal {
	return JObj().
		Set("id", JInt(int64(comment.ID))).
		Set("body", JStr(comment.Body)).
		Set("user", userObject()).
		Set("created_at", JStr(comment.CreatedAt)).
		Set("updated_at", JStr(comment.UpdatedAt))
}

func issueObject(store *Store, repo *Repo, item *Item) *JVal {
	labels := JArr()
	for _, label := range item.Labels {
		labels.Arr = append(labels.Arr, labelObject(label))
	}
	closedAt := JNul()
	if item.ClosedAt != "" {
		closedAt = JStr(item.ClosedAt)
	}
	object := JObj().
		Set("id", JInt(int64(item.ID))).
		Set("number", JInt(int64(item.Number))).
		Set("title", JStr(item.Title)).
		Set("body", JStr(item.Body)).
		Set("state", JStr(item.State)).
		Set("labels", labels).
		Set("user", userObject()).
		Set("assignees", JArr()).
		Set("milestone", JNul()).
		Set("locked", JBool(false)).
		Set("comments", JInt(int64(len(item.Comments)))).
		Set("created_at", JStr(item.CreatedAt)).
		Set("updated_at", JStr(item.UpdatedAt)).
		Set("closed_at", closedAt)
	if item.Kind == KindPull {
		mergedAt := JNul()
		if item.MergedAt != "" {
			mergedAt = JStr(item.MergedAt)
		}
		object.Set("pull_request", JObj().Set("merged_at", mergedAt))
	}
	return object
}

func pullObject(store *Store, repo *Repo, item *Item) (*JVal, error) {
	headSha := ZeroOID
	if item.HeadRef != "" {
		sha, ok, err := store.ResolveRef(repo, item.HeadRef)
		if err != nil {
			return nil, err
		}
		if ok {
			headSha = sha
		}
	}
	baseSha := ZeroOID
	if item.BaseRef != "" {
		sha, ok, err := store.ResolveRef(repo, item.BaseRef)
		if err != nil {
			return nil, err
		}
		if ok {
			baseSha = sha
		}
	}
	mergeable, err := store.IsMergeable(repo, item)
	if err != nil {
		return nil, err
	}
	mergeCommitSha := JNul()
	if item.MergeCommitSha != "" {
		mergeCommitSha = JStr(item.MergeCommitSha)
	}
	mergedAt := JNul()
	if item.MergedAt != "" {
		mergedAt = JStr(item.MergedAt)
	}
	object := issueObject(store, repo, item)
	object.Set("head", JObj().
		Set("label", JStr(repo.Owner+":"+item.HeadRef)).
		Set("ref", JStr(item.HeadRef)).
		Set("sha", JStr(headSha)).
		Set("repo", repoObject(store, repo)).
		Set("user", userObject()))
	object.Set("base", JObj().
		Set("label", JStr(repo.Owner+":"+item.BaseRef)).
		Set("ref", JStr(item.BaseRef)).
		Set("sha", JStr(baseSha)).
		Set("repo", repoObject(store, repo)).
		Set("user", userObject()))
	object.Set("merged", JBool(item.Merged))
	object.Set("mergeable", JBool(mergeable))
	object.Set("merge_commit_sha", mergeCommitSha)
	object.Set("merged_at", mergedAt)
	object.Set("draft", JBool(false))
	return object, nil
}

var identityPattern = regexp.MustCompile(`^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$`)

// identityFields parses a commit's `Name <email> ts tz` identity line into
// GitHub author fields. An unparseable line keeps the whole identity as the
// name, an empty email, and the trial clock.
func identityFields(identity string) (name, email, date string, err error) {
	match := identityPattern.FindStringSubmatch(trimJSSpace(identity))
	if match == nil {
		return identity, "", now.iso, nil
	}
	epoch, _ := strconv.ParseFloat(match[3], 64)
	iso, ok := jsISODate(epoch)
	if !ok {
		return "", "", "", errIdentityDate
	}
	return match[1], match[2], iso, nil
}

var errIdentityDate = errors.New("identity date out of range")

func commitObject(store *Store, repo *Repo, sha string) (*JVal, error) {
	view := store.CommitView(repo, sha)
	if view == nil {
		return nil, nil
	}
	authorName, authorEmail, authorDate, err := identityFields(view.Author)
	if err != nil {
		return nil, err
	}
	committerName, committerEmail, committerDate, err := identityFields(view.Committer)
	if err != nil {
		return nil, err
	}
	parents := JArr()
	for _, parent := range view.Parents {
		parents.Arr = append(parents.Arr, JObj().Set("sha", JStr(parent)))
	}
	return JObj().
		Set("sha", JStr(sha)).
		Set("commit", JObj().
			Set("message", JStr(view.Message)).
			Set("author", JObj().Set("name", JStr(authorName)).Set("email", JStr(authorEmail)).Set("date", JStr(authorDate))).
			Set("committer", JObj().Set("name", JStr(committerName)).Set("email", JStr(committerEmail)).Set("date", JStr(committerDate))).
			Set("tree", JObj().Set("sha", JStr(view.Tree)))).
		Set("author", userObject()).
		Set("committer", userObject()).
		Set("parents", parents), nil
}

func refObject(store *Store, repo *Repo, refname, oid string) *JVal {
	typ := "commit"
	if object := store.GetObject(repo, oid); object != nil {
		typ = string(object.Type)
	}
	return JObj().
		Set("ref", JStr(refname)).
		Set("object", JObj().Set("sha", JStr(oid)).Set("type", JStr(typ)))
}

// ---------------------------------------------------------------------------
// Pagination
// ---------------------------------------------------------------------------

// paginate slices items to one page and builds the Link header when there
// is more than one page. per_page parses as JavaScript's parseInt does and
// defaults to 30 outside 1..100; page defaults to 1 below 1.
func paginate[T any](items []T, query queryParams, host, path string) ([]T, string, bool) {
	perPage := 30
	if v, ok := parseLeadingInt(query.get("per_page")); ok && isSafeInteger(v) && v >= 1 && v <= 100 {
		perPage = int(v)
	}
	page := 1
	if v, ok := parseLeadingInt(query.get("page")); ok && isSafeInteger(v) && v >= 1 {
		page = int(v)
	}
	totalPages := (len(items) + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	var slice []T
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = []T{}
	}
	if totalPages <= 1 {
		return slice, "", false
	}
	build := func(target int) string {
		params := queryParams{pairs: append([]queryPair(nil), query.pairs...)}
		params.set("page", strconv.Itoa(target))
		params.set("per_page", strconv.Itoa(perPage))
		return "http://" + host + path + "?" + params.String()
	}
	links := []string{
		"<" + build(1) + `>; rel="first"`,
		"<" + build(totalPages) + `>; rel="last"`,
	}
	if page > 1 {
		links = append(links, "<"+build(page-1)+`>; rel="prev"`)
	}
	if page < totalPages {
		links = append(links, "<"+build(page+1)+`>; rel="next"`)
	}
	return slice, strings.Join(links, ", "), true
}

// parseLeadingInt parses the leading integer of s the way JavaScript's
// parseInt with radix 10 does: leading whitespace is skipped, an optional
// sign and a run of digits follow, trailing garbage is ignored. It reports
// false when no digit run follows.
func parseLeadingInt(s string) (float64, bool) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isJSSpace(r) {
			break
		}
		i += size
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	acc := 0.0
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
	return acc, true
}

func isSafeInteger(v float64) bool {
	return v == float64(int64(v)) && v <= 9007199254740991 && v >= -9007199254740991
}

// ---------------------------------------------------------------------------
// Body helpers
// ---------------------------------------------------------------------------

// bodyObject requires the parsed body to be an object; a missing or null
// body reads as an empty object, anything else is a 400.
func bodyObject(body *JVal) (*JVal, error) {
	if body == nil || body.IsNull() {
		return JObj(), nil
	}
	if body.Kind == KObj {
		return body, nil
	}
	return nil, newStoreError(400, "Problems parsing JSON")
}

func requiredString(body *JVal, field string) (string, error) {
	value, ok := body.Get(field).AsStr()
	if !ok || value == "" {
		return "", newStoreError(422, "%s is required", field)
	}
	return value, nil
}

func optionalString(body *JVal, field, fallback string) string {
	if value, ok := body.Get(field).AsStr(); ok {
		return value
	}
	return fallback
}

func optionalBoolean(body *JVal, field string, fallback bool) bool {
	if value, ok := body.Get(field).AsBool(); ok {
		return value
	}
	return fallback
}

func stringArray(body *JVal, field string) ([]string, error) {
	value := body.Get(field)
	if value == nil {
		return nil, nil
	}
	if value.Kind != KArr {
		return nil, newStoreError(422, "%s must be an array of strings", field)
	}
	out := make([]string, 0, len(value.Arr))
	for _, entry := range value.Arr {
		s, ok := entry.AsStr()
		if !ok {
			return nil, newStoreError(422, "%s must be an array of strings", field)
		}
		out = append(out, s)
	}
	return out, nil
}

func stringField(body *JVal, field string) (string, bool) {
	return body.Get(field).AsStr()
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

// applyContentChange builds the new tree, commits it on the branch, and
// moves the branch. It returns the new commit's sha and the blob's sha (the
// empty string when the file was deleted).
func applyContentChange(store *Store, repo *Repo, branch, path, message string, blob []byte, isDelete bool, author, committer string) (string, string, error) {
	refname := "refs/heads/" + branch
	tip, hasTip := store.GetRef(repo, refname)
	var files FlatTree
	if hasTip {
		files = store.FlatTreeOf(repo, tip)
	} else {
		files = FlatTree{Files: map[string]FlatFile{}}
	}
	blobSha := ""
	if isDelete {
		delete(files.Files, path)
	} else {
		blobSha = store.PutObject(repo, ObjBlob, blob)
		files.Files[path] = FlatFile{OID: blobSha, Mode: "100644"}
	}
	files.Paths = make([]string, 0, len(files.Files))
	for p := range files.Files {
		files.Paths = append(files.Paths, p)
	}
	sort.Strings(files.Paths)
	tree, err := store.BuildTreeFromFlat(repo, files)
	if err != nil {
		return "", "", err
	}
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitSha, err := store.CreateCommit(repo, tree, parents, message, author, committer)
	if err != nil {
		return "", "", err
	}
	store.SetRef(repo, refname, commitSha)
	repo.UpdatedAt = now.iso
	return commitSha, blobSha, nil
}

// identityFrom reads a `{name, email}` identity object from a body field.
func identityFrom(body *JVal, field string) (string, bool) {
	value := body.Get(field)
	if value == nil || value.Kind != KObj {
		return "", false
	}
	name, ok1 := value.Get("name").AsStr()
	email, ok2 := value.Get("email").AsStr()
	if !ok1 || !ok2 {
		return "", false
	}
	return name + " <" + email + ">", true
}

// decodeBase64Lenient decodes base64 the way Node's Buffer.from does: all
// whitespace is stripped first, characters outside the alphabet are
// skipped, '=' ends the data, and a short final group yields its bytes.
func decodeBase64Lenient(content string) []byte {
	var values []byte
	for _, r := range content {
		if isJSSpace(r) {
			continue
		}
		switch {
		case r >= 'A' && r <= 'Z':
			values = append(values, byte(r-'A'))
		case r >= 'a' && r <= 'z':
			values = append(values, byte(r-'a')+26)
		case r >= '0' && r <= '9':
			values = append(values, byte(r-'0')+52)
		case r == '+':
			values = append(values, 62)
		case r == '/':
			values = append(values, 63)
		case r == '=':
			// Padding ends the data; anything after it is ignored.
			goto decode
		default:
			// Characters outside the alphabet are skipped.
		}
	}
decode:
	out := make([]byte, 0, len(values)*3/4+3)
	for i := 0; i+4 <= len(values); i += 4 {
		out = append(out, values[i]<<2|values[i+1]>>4, values[i+1]<<4|values[i+2]>>2, values[i+2]<<6|values[i+3])
	}
	switch rem := len(values) % 4; rem {
	case 3:
		tail := values[len(values)-3:]
		out = append(out, tail[0]<<2|tail[1]>>4, tail[1]<<4|tail[2]>>2)
	case 2:
		tail := values[len(values)-2:]
		out = append(out, tail[0]<<2|tail[1]>>4)
	}
	return out
}

// ---------------------------------------------------------------------------
// Router
// ---------------------------------------------------------------------------

// handleRest routes one authenticated REST request. The transport layer has
// already checked the bearer token and parsed the JSON body (a malformed
// body never reaches here: it is a 400 there). StoreErrors carry their HTTP
// status; anything else is a 500 the dispatcher turns into a JSON error.
func handleRest(store *Store, req restRequest) (handlerReply, error) {
	segments := req.segments
	method := req.method
	var first, second, third string
	if len(segments) > 0 {
		first = segments[0]
	}
	if len(segments) > 1 {
		second = segments[1]
	}
	if len(segments) > 2 {
		third = segments[2]
	}

	// Root and meta.
	if len(segments) == 0 {
		if method != "GET" {
			return restNotFound(store), nil
		}
		return jsonReply(store, 200,
			JObj().Set("current_user_url", JStr("/api/v3/user")).Set("rate_limit_url", JStr("/api/v3/rate_limit")),
			[2]string{"x-oauth-scopes", "repo, read:org"}), nil
	}
	if first == "meta" && len(segments) == 1 {
		if method != "GET" {
			return restNotFound(store), nil
		}
		return jsonReply(store, 200, JObj().Set("installed_version", JStr("1.0.0"))), nil
	}
	if first == "user" && len(segments) == 1 {
		if method != "GET" {
			return restNotFound(store), nil
		}
		return jsonReply(store, 200, userObject()), nil
	}
	if first == "user" && second == "repos" && len(segments) == 2 {
		if method == "GET" {
			page, link, hasLink := paginate(store.ListRepos(), req.query, req.host, "/api/v3/user/repos")
			arr := JArr()
			for _, repo := range page {
				arr.Arr = append(arr.Arr, repoObject(store, repo))
			}
			var extras [][2]string
			if hasLink {
				extras = [][2]string{{"link", link}}
			}
			return jsonReply(store, 200, arr, extras...), nil
		}
		if method == "POST" {
			body, err := bodyObject(req.body)
			if err != nil {
				return handlerReply{}, err
			}
			name, err := requiredString(body, "name")
			if err != nil {
				return handlerReply{}, err
			}
			repo, err := store.CreateRepo(name, optionalString(body, "description", ""), optionalBoolean(body, "private", false))
			if err != nil {
				return handlerReply{}, err
			}
			return jsonReply(store, 201, repoObject(store, repo)), nil
		}
		return restNotFound(store), nil
	}

	// Repository-scoped routes.
	if first == "repos" && len(segments) >= 3 {
		owner, name := second, third
		repo := store.GetRepo(owner, name)
		if repo == nil {
			return restNotFound(store), nil
		}
		tail := segments[3:]

		if len(tail) == 0 {
			if method == "GET" {
				return jsonReply(store, 200, repoObject(store, repo)), nil
			}
			if method == "PATCH" {
				body, err := bodyObject(req.body)
				if err != nil {
					return handlerReply{}, err
				}
				if v, ok := stringField(body, "description"); ok {
					repo.Description = v
				}
				if v, ok := body.Get("private").AsBool(); ok {
					repo.IsPrivate = v
				}
				repo.UpdatedAt = now.iso
				return jsonReply(store, 200, repoObject(store, repo)), nil
			}
			if method == "DELETE" {
				store.DeleteRepo(repo)
				return handlerReply{
					status:  204,
					headers: [][2]string{{"x-github-request-id", store.NextRequestID()}},
					body:    nil,
				}, nil
			}
			return restNotFound(store), nil
		}

		resource := tail[0]
		resourceTail := tail[1:]

		if resource == "branches" {
			if method != "GET" {
				return restNotFound(store), nil
			}
			if len(resourceTail) == 0 {
				type branch struct {
					name string
					sha  string
				}
				var branches []branch
				for _, ref := range store.ListRefs(repo) {
					if strings.HasPrefix(ref.Name, "refs/heads/") {
						branches = append(branches, branch{ref.Name[len("refs/heads/"):], ref.OID})
					}
				}
				sort.Slice(branches, func(i, j int) bool { return branches[i].name < branches[j].name })
				arr := JArr()
				for _, b := range branches {
					arr.Arr = append(arr.Arr, JObj().Set("name", JStr(b.name)).Set("commit", JObj().Set("sha", JStr(b.sha))).Set("protected", JBool(false)))
				}
				return jsonReply(store, 200, arr), nil
			}
			branch := strings.Join(resourceTail, "/")
			oid, ok := store.GetRef(repo, "refs/heads/"+branch)
			if !ok {
				return restNotFound(store), nil
			}
			return jsonReply(store, 200, JObj().Set("name", JStr(branch)).Set("commit", JObj().Set("sha", JStr(oid))).Set("protected", JBool(false))), nil
		}

		if resource == "commits" {
			if method != "GET" || len(resourceTail) != 0 {
				return restNotFound(store), nil
			}
			ref := repo.DefaultBranch
			if req.query.has("sha") {
				ref = req.query.get("sha")
			}
			tip, ok, err := store.ResolveRef(repo, ref)
			if err != nil {
				return handlerReply{}, err
			}
			if !ok {
				return restNotFound(store), nil
			}
			arr := JArr()
			for _, sha := range store.CommitHistory(repo, tip, 1000) {
				committed, err := commitObject(store, repo, sha)
				if err != nil {
					return handlerReply{}, err
				}
				if committed != nil {
					arr.Arr = append(arr.Arr, committed)
				}
			}
			return jsonReply(store, 200, arr), nil
		}

		if resource == "git" && len(resourceTail) > 0 && resourceTail[0] == "refs" {
			refTail := resourceTail[1:]
			if method == "POST" && len(refTail) == 0 {
				body, err := bodyObject(req.body)
				if err != nil {
					return handlerReply{}, err
				}
				ref, err := requiredString(body, "ref")
				if err != nil {
					return handlerReply{}, err
				}
				sha, err := requiredString(body, "sha")
				if err != nil {
					return handlerReply{}, err
				}
				if !strings.HasPrefix(ref, "refs/") {
					return handlerReply{}, newStoreError(422, "ref must start with refs/")
				}
				if !IsObjectID(sha) || store.GetObject(repo, sha) == nil {
					return handlerReply{}, newStoreError(422, "object does not exist")
				}
				if _, exists := store.GetRef(repo, ref); exists {
					return handlerReply{}, newStoreError(422, "Reference already exists")
				}
				store.SetRef(repo, ref, sha)
				return jsonReply(store, 201, refObject(store, repo, ref, sha)), nil
			}
			if method == "GET" {
				if len(refTail) == 0 {
					arr := JArr()
					for _, ref := range store.ListRefs(repo) {
						arr.Arr = append(arr.Arr, refObject(store, repo, ref.Name, ref.OID))
					}
					return jsonReply(store, 200, arr), nil
				}
				wanted := "refs/" + strings.Join(refTail, "/")
				if exact, ok := store.GetRef(repo, wanted); ok {
					return jsonReply(store, 200, refObject(store, repo, wanted, exact)), nil
				}
				arr := JArr()
				for _, ref := range store.ListRefs(repo) {
					if strings.HasPrefix(ref.Name, wanted+"/") || ref.Name == wanted {
						arr.Arr = append(arr.Arr, refObject(store, repo, ref.Name, ref.OID))
					}
				}
				if len(arr.Arr) == 0 {
					return restNotFound(store), nil
				}
				return jsonReply(store, 200, arr), nil
			}
			return restNotFound(store), nil
		}

		if resource == "contents" {
			path := strings.Join(resourceTail, "/")
			if method == "GET" {
				ref := repo.DefaultBranch
				if req.query.has("ref") {
					ref = req.query.get("ref")
				}
				tip, ok, err := store.ResolveRef(repo, ref)
				if err != nil {
					return handlerReply{}, err
				}
				if !ok {
					return restNotFound(store), nil
				}
				var found *PathTarget
				if path == "" {
					oid := ""
					if view := store.CommitView(repo, tip); view != nil {
						oid = view.Tree
					}
					found = &PathTarget{IsTree: true, OID: oid, Mode: "40000", Name: ""}
				} else {
					found = store.ReadPath(repo, tip, path)
				}
				if found == nil {
					return restNotFound(store), nil
				}
				if found.IsTree {
					arr := JArr()
					for _, entry := range store.TreeEntries(repo, found.OID) {
						entryPath := entry.Name
						if path != "" {
							entryPath = path + "/" + entry.Name
						}
						size := 0
						typ := "file"
						if entry.Mode == "40000" {
							typ = "dir"
						} else if obj := store.GetObject(repo, entry.OID); obj != nil {
							size = len(obj.Data)
						}
						arr.Arr = append(arr.Arr, JObj().
							Set("type", JStr(typ)).
							Set("name", JStr(entry.Name)).
							Set("path", JStr(entryPath)).
							Set("sha", JStr(entry.OID)).
							Set("size", JInt(int64(size))))
					}
					return jsonReply(store, 200, arr), nil
				}
				blob := store.GetObject(repo, found.OID)
				if blob == nil {
					return restNotFound(store), nil
				}
				return jsonReply(store, 200, JObj().
					Set("type", JStr("file")).
					Set("encoding", JStr("base64")).
					Set("size", JInt(int64(len(blob.Data)))).
					Set("name", JStr(found.Name)).
					Set("path", JStr(path)).
					Set("content", JStr(encodeBase64(blob.Data))).
					Set("sha", JStr(found.OID))), nil
			}
			if method == "PUT" || method == "DELETE" {
				if path == "" {
					return restNotFound(store), nil
				}
				body, err := bodyObject(req.body)
				if err != nil {
					return handlerReply{}, err
				}
				message, err := requiredString(body, "message")
				if err != nil {
					return handlerReply{}, err
				}
				branch := optionalString(body, "branch", repo.DefaultBranch)
				sha, shaPresent := stringField(body, "sha")
				tip, tipOK, err := store.ResolveRef(repo, branch)
				if err != nil {
					return handlerReply{}, err
				}
				var current *PathTarget
				if tipOK {
					current = store.ReadPath(repo, tip, path)
				}
				if method == "DELETE" {
					if current == nil || current.IsTree {
						return restNotFound(store), nil
					}
					if shaPresent && sha != current.OID {
						return handlerReply{}, newStoreError(409, "The content SHA is stale")
					}
					committer, _ := identityFrom(body, "committer")
					if committer == "" {
						committer = viewerIdentity()
					}
					author, _ := identityFrom(body, "author")
					if author == "" {
						author = committer
					}
					commitSha, _, err := applyContentChange(store, repo, branch, path, message, nil, true, author, committer)
					if err != nil {
						return handlerReply{}, err
					}
					return jsonReply(store, 200, JObj().Set("commit", JObj().Set("sha", JStr(commitSha)))), nil
				}
				content, ok := stringField(body, "content")
				if !ok {
					return handlerReply{}, newStoreError(422, "content is required")
				}
				blob := decodeBase64Lenient(content)
				exists := current != nil && !current.IsTree
				if exists && (!shaPresent || sha != current.OID) {
					return handlerReply{}, newStoreError(409, "The content SHA is stale")
				}
				committer, _ := identityFrom(body, "committer")
				if committer == "" {
					committer = viewerIdentity()
				}
				author, _ := identityFrom(body, "author")
				if author == "" {
					author = committer
				}
				commitSha, blobSha, err := applyContentChange(store, repo, branch, path, message, blob, false, author, committer)
				if err != nil {
					return handlerReply{}, err
				}
				name := path
				if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
					name = path[slash+1:]
				}
				status := 201
				if exists {
					status = 200
				}
				return jsonReply(store, status, JObj().
					Set("content", JObj().
						Set("name", JStr(name)).
						Set("path", JStr(path)).
						Set("sha", JStr(blobSha)).
						Set("size", JInt(int64(len(blob)))).
						Set("encoding", JStr("base64"))).
					Set("commit", JObj().Set("sha", JStr(commitSha)))), nil
			}
			return restNotFound(store), nil
		}

		if resource == "labels" {
			if len(resourceTail) != 0 {
				return restNotFound(store), nil
			}
			if method == "GET" {
				arr := JArr()
				for _, label := range repo.Labels {
					arr.Arr = append(arr.Arr, labelObject(label))
				}
				return jsonReply(store, 200, arr), nil
			}
			if method == "POST" {
				body, err := bodyObject(req.body)
				if err != nil {
					return handlerReply{}, err
				}
				name, err := requiredString(body, "name")
				if err != nil {
					return handlerReply{}, err
				}
				label, err := store.CreateLabel(repo, name, optionalString(body, "color", "ededed"), optionalString(body, "description", ""))
				if err != nil {
					return handlerReply{}, err
				}
				return jsonReply(store, 201, labelObject(label)), nil
			}
			return restNotFound(store), nil
		}

		if resource == "issues" {
			if len(resourceTail) == 0 {
				if method == "GET" {
					state := "open"
					if req.query.has("state") {
						state = req.query.get("state")
					}
					direction := "desc"
					if req.query.has("direction") {
						direction = req.query.get("direction")
					}
					items := store.ListItems(repo)
					if state != "all" {
						filtered := items[:0]
						for _, item := range items {
							if item.State == state {
								filtered = append(filtered, item)
							}
						}
						items = filtered
					}
					sorted := append([]*Item(nil), items...)
					if direction == "asc" {
						sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
					} else {
						sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number > sorted[j].Number })
					}
					page, link, hasLink := paginate(sorted, req.query, req.host, "/api/v3/repos/"+owner+"/"+name+"/issues")
					arr := JArr()
					for _, item := range page {
						arr.Arr = append(arr.Arr, issueObject(store, repo, item))
					}
					var extras [][2]string
					if hasLink {
						extras = [][2]string{{"link", link}}
					}
					return jsonReply(store, 200, arr, extras...), nil
				}
				if method == "POST" {
					body, err := bodyObject(req.body)
					if err != nil {
						return handlerReply{}, err
					}
					title, err := requiredString(body, "title")
					if err != nil {
						return handlerReply{}, err
					}
					labelNames, err := stringArray(body, "labels")
					if err != nil {
						return handlerReply{}, err
					}
					item, err := store.CreateItem(repo, KindIssue, title, optionalString(body, "body", ""), labelNames, "", "")
					if err != nil {
						return handlerReply{}, err
					}
					return jsonReply(store, 201, issueObject(store, repo, item)), nil
				}
				return restNotFound(store), nil
			}
			number, ok := parseIssueNumber(resourceTail[0])
			if !ok {
				return restNotFound(store), nil
			}
			item := store.GetItem(repo, number)
			if item == nil {
				return restNotFound(store), nil
			}
			if len(resourceTail) == 1 {
				if method == "GET" {
					// The read workload hammers this route: serve a
					// cached serialization when the repository has
					// not changed since it was built.
					if cached := store.CachedIssueBody(repo, item); cached != nil {
						return handlerReply{
							status: 200,
							headers: [][2]string{
								{"content-type", jsonType},
								{"x-github-request-id", store.NextRequestID()},
							},
							body: cached,
						}, nil
					}
					built := jsonReply(store, 200, issueObject(store, repo, item))
					store.CacheIssueBody(repo, item, built.body)
					return built, nil
				}
				if method == "PATCH" {
					body, err := bodyObject(req.body)
					if err != nil {
						return handlerReply{}, err
					}
					if v, ok := stringField(body, "title"); ok {
						item.Title = v
					}
					if v, ok := stringField(body, "body"); ok {
						item.Body = v
					}
					if state, ok := stringField(body, "state"); ok && (state == "open" || state == "closed") {
						item.State = state
						if state == "closed" {
							item.ClosedAt = now.iso
						} else {
							item.ClosedAt = ""
						}
					}
					if body.Get("labels") != nil {
						names, err := stringArray(body, "labels")
						if err != nil {
							return handlerReply{}, err
						}
						var labels []*Label
						for _, name := range names {
							label := store.GetLabel(repo, name)
							if label == nil {
								return handlerReply{}, newStoreError(422, "Label %s does not exist", name)
							}
							labels = append(labels, label)
						}
						item.Labels = labels
					}
					item.UpdatedAt = now.iso
					store.Touch(repo)
					return jsonReply(store, 200, issueObject(store, repo, item)), nil
				}
				return restNotFound(store), nil
			}
			if len(resourceTail) == 2 && resourceTail[1] == "comments" {
				if method == "GET" {
					arr := JArr()
					for _, comment := range item.Comments {
						arr.Arr = append(arr.Arr, commentObject(comment))
					}
					return jsonReply(store, 200, arr), nil
				}
				if method == "POST" {
					body, err := bodyObject(req.body)
					if err != nil {
						return handlerReply{}, err
					}
					text, err := requiredString(body, "body")
					if err != nil {
						return handlerReply{}, err
					}
					comment := store.AddComment(repo, item, text)
					return jsonReply(store, 201, commentObject(comment)), nil
				}
				return restNotFound(store), nil
			}
			return restNotFound(store), nil
		}

		if resource == "pulls" {
			if len(resourceTail) == 0 {
				if method == "GET" {
					state := "open"
					if req.query.has("state") {
						state = req.query.get("state")
					}
					var items []*Item
					for _, item := range store.ListItems(repo) {
						if item.Kind == KindPull {
							items = append(items, item)
						}
					}
					if state != "all" {
						filtered := items[:0]
						for _, item := range items {
							if item.State == state {
								filtered = append(filtered, item)
							}
						}
						items = filtered
					}
					sort.Slice(items, func(i, j int) bool { return items[i].Number > items[j].Number })
					page, link, hasLink := paginate(items, req.query, req.host, "/api/v3/repos/"+owner+"/"+name+"/pulls")
					arr := JArr()
					for _, item := range page {
						pulled, err := pullObject(store, repo, item)
						if err != nil {
							return handlerReply{}, err
						}
						arr.Arr = append(arr.Arr, pulled)
					}
					var extras [][2]string
					if hasLink {
						extras = [][2]string{{"link", link}}
					}
					return jsonReply(store, 200, arr, extras...), nil
				}
				if method == "POST" {
					body, err := bodyObject(req.body)
					if err != nil {
						return handlerReply{}, err
					}
					head, err := requiredString(body, "head")
					if err != nil {
						return handlerReply{}, err
					}
					base, err := requiredString(body, "base")
					if err != nil {
						return handlerReply{}, err
					}
					if _, ok, err := store.ResolveRef(repo, head); err != nil {
						return handlerReply{}, err
					} else if !ok {
						return handlerReply{}, newStoreError(422, "Head branch %s does not exist", head)
					}
					if _, ok, err := store.ResolveRef(repo, base); err != nil {
						return handlerReply{}, err
					} else if !ok {
						return handlerReply{}, newStoreError(422, "Base branch %s does not exist", base)
					}
					title, err := requiredString(body, "title")
					if err != nil {
						return handlerReply{}, err
					}
					labelNames, err := stringArray(body, "labels")
					if err != nil {
						return handlerReply{}, err
					}
					item, err := store.CreateItem(repo, KindPull, title, optionalString(body, "body", ""), labelNames, head, base)
					if err != nil {
						return handlerReply{}, err
					}
					pulled, err := pullObject(store, repo, item)
					if err != nil {
						return handlerReply{}, err
					}
					return jsonReply(store, 201, pulled), nil
				}
				return restNotFound(store), nil
			}
			number, ok := parseIssueNumber(resourceTail[0])
			if !ok {
				return restNotFound(store), nil
			}
			item := store.GetItem(repo, number)
			if item == nil || item.Kind != KindPull {
				return restNotFound(store), nil
			}
			if len(resourceTail) == 1 {
				if method == "GET" {
					pulled, err := pullObject(store, repo, item)
					if err != nil {
						return handlerReply{}, err
					}
					return jsonReply(store, 200, pulled), nil
				}
				return restNotFound(store), nil
			}
			if len(resourceTail) == 2 && resourceTail[1] == "files" {
				if method != "GET" {
					return restNotFound(store), nil
				}
				baseTip, baseOK, err := store.ResolveRef(repo, item.BaseRef)
				if err != nil {
					return handlerReply{}, err
				}
				headTip, headOK, err := store.ResolveRef(repo, item.HeadRef)
				if err != nil {
					return handlerReply{}, err
				}
				if !baseOK || !headOK {
					return restNotFound(store), nil
				}
				base, hasBase := store.MergeBase(repo, baseTip, headTip)
				arr := JArr()
				for _, file := range store.DiffFiles(repo, base, hasBase, headTip) {
					arr.Arr = append(arr.Arr, JObj().
						Set("filename", JStr(file.Filename)).
						Set("status", JStr(file.Status)).
						Set("additions", JInt(int64(file.Additions))).
						Set("deletions", JInt(int64(file.Deletions))).
						Set("changes", JInt(int64(file.Additions+file.Deletions))))
				}
				return jsonReply(store, 200, arr), nil
			}
			if len(resourceTail) == 2 && resourceTail[1] == "merge" {
				if method != "PUT" {
					return restNotFound(store), nil
				}
				if item.State != "open" {
					return handlerReply{}, newStoreError(405, "Pull request is already closed")
				}
				body, err := bodyObject(req.body)
				if err != nil {
					return handlerReply{}, err
				}
				title := optionalString(body, "commit_title", item.Title+" (#"+strconv.Itoa(item.Number)+")")
				extra := optionalString(body, "commit_message", "")
				message := title
				if extra != "" && extra != title {
					message = title + "\n\n" + extra
				}
				sha, err := store.MergePullRequest(repo, item, message)
				if err != nil {
					return handlerReply{}, err
				}
				if sha == "" {
					return handlerReply{}, newStoreError(405, "Pull request cannot be merged due to conflicts")
				}
				return jsonReply(store, 200, JObj().
					Set("sha", JStr(sha)).
					Set("merged", JBool(true)).
					Set("message", JStr("Pull request successfully merged"))), nil
			}
			return restNotFound(store), nil
		}

		return restNotFound(store), nil
	}

	return restNotFound(store), nil
}

// parseIssueNumber parses an issue/PR number the way JavaScript's Number()
// does: the value must be a safe integer of at least 1.
func parseIssueNumber(raw string) (int, bool) {
	v, ok := ParseJSNumber(raw)
	if !ok || !isSafeInteger(v) || v < 1 {
		return 0, false
	}
	return int(v), true
}

// encodeBase64 renders bytes as standard padded base64.
func encodeBase64(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	b.Grow((len(data) + 2) / 3 * 4)
	for i := 0; i < len(data); i += 3 {
		var n uint32
		rem := len(data) - i
		n = uint32(data[i]) << 16
		if rem > 1 {
			n |= uint32(data[i+1]) << 8
		}
		if rem > 2 {
			n |= uint32(data[i+2])
		}
		b.WriteByte(alphabet[(n>>18)&63])
		b.WriteByte(alphabet[(n>>12)&63])
		if rem > 1 {
			b.WriteByte(alphabet[(n>>6)&63])
		} else {
			b.WriteByte('=')
		}
		if rem > 2 {
			b.WriteByte(alphabet[n&63])
		} else {
			b.WriteByte('=')
		}
	}
	return b.String()
}
