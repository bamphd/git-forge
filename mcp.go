// The MCP surface: JSON-RPC 2.0 over Streamable HTTP at POST /mcp,
// answering plain JSON (never SSE). Both protocol eras the suite accepts
// are served unconditionally: the modern 2026-07-28 `server/discover`
// (identity, capabilities and supported versions in one answer, no
// handshake, no session) and the legacy `initialize` +
// `notifications/initialized` handshake. Every request is stateless and
// answered from the one store, so MCP, REST, git and GraphQL always agree.
//
// Wire rules, as the suite asserts them: `tools/list` answers the same six
// tools byte-for-byte on every call (a single static list, one page, no
// cursor); an unknown tool is JSON-RPC -32602; a body that is not JSON is
// HTTP 400 with JSON-RPC -32700; a tools/call missing required arguments
// is JSON-RPC -32602; notifications (no id) are accepted with HTTP 202 and
// no body. Tool results carry both a JSON text block and structuredContent
// holding the same REST-shaped object, so the suite can read them either
// way. Authentication is the transport layer's job.

package main

import (
	"errors"
	"sort"
	"strings"
)

const mcpServerName = "git-forge"
const mcpServerVersion = "1.0.0"

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25"}

const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
)

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

type mcpToolDef struct {
	name        string
	description string
	properties  *JVal // ordered object: property name -> schema object
	required    []string
}

func mcpProp(propType, description string, enumValues ...string) *JVal {
	prop := JObj().Set("type", JStr(propType))
	if len(enumValues) > 0 {
		arr := JArr()
		for _, v := range enumValues {
			arr.Arr = append(arr.Arr, JStr(v))
		}
		prop.Set("enum", arr)
	}
	return prop.Set("description", JStr(description))
}

var mcpTools = []mcpToolDef{
	{
		name:        "create_repo",
		description: "Create a new repository owned by the authenticated user.",
		properties: JObj().
			Set("name", mcpProp("string", "The repository name.")).
			Set("description", mcpProp("string", "The repository description.")).
			Set("private", mcpProp("boolean", "Whether the repository is private.")),
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Read one repository by owner and name.",
		properties: JObj().
			Set("owner", mcpProp("string", "The repository owner login.")).
			Set("repo", mcpProp("string", "The repository name.")),
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: JObj().
			Set("owner", mcpProp("string", "The repository owner login.")).
			Set("repo", mcpProp("string", "The repository name.")).
			Set("title", mcpProp("string", "The issue title.")).
			Set("body", mcpProp("string", "The issue body.")),
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository.",
		properties: JObj().
			Set("owner", mcpProp("string", "The repository owner login.")).
			Set("repo", mcpProp("string", "The repository name.")).
			Set("state", mcpProp("string", "Filter by issue state.", "open", "closed", "all")),
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch into a base branch.",
		properties: JObj().
			Set("owner", mcpProp("string", "The repository owner login.")).
			Set("repo", mcpProp("string", "The repository name.")).
			Set("title", mcpProp("string", "The pull request title.")).
			Set("body", mcpProp("string", "The pull request body.")).
			Set("head", mcpProp("string", "The head branch name.")).
			Set("base", mcpProp("string", "The base branch name.")),
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request into its base branch.",
		properties: JObj().
			Set("owner", mcpProp("string", "The repository owner login.")).
			Set("repo", mcpProp("string", "The repository name.")).
			Set("pull_number", mcpProp("integer", "The pull request number.")),
		required: []string{"owner", "repo", "pull_number"},
	},
}

// mcpToolsListResult is the one tools/list payload, built once and
// serialized identically on every call.
var mcpToolsListResult = func() *JVal {
	tools := JArr()
	for _, tool := range mcpTools {
		required := JArr()
		for _, name := range tool.required {
			required.Arr = append(required.Arr, JStr(name))
		}
		schema := JObj().
			Set("type", JStr("object")).
			Set("properties", tool.properties).
			Set("required", required).
			Set("additionalProperties", JBool(false))
		tools.Arr = append(tools.Arr, JObj().
			Set("name", JStr(tool.name)).
			Set("description", JStr(tool.description)).
			Set("inputSchema", schema))
	}
	return JObj().Set("tools", tools)
}()

// ---------------------------------------------------------------------------
// Replies
// ---------------------------------------------------------------------------

func mcpJSONReply(store *Store, status int, value *JVal) handlerReply {
	body := AppendJSON(nil, value)
	body = append(body, '\n')
	return handlerReply{
		status: status,
		headers: [][2]string{
			{"content-type", jsonType},
			{"x-github-request-id", store.NextRequestID()},
		},
		body: body,
	}
}

func mcpResult(store *Store, id *JVal, result *JVal) handlerReply {
	return mcpJSONReply(store, 200, JObj().
		Set("jsonrpc", JStr("2.0")).
		Set("id", id).
		Set("result", result))
}

func mcpError(store *Store, id *JVal, code int, message string, status ...int) handlerReply {
	codeStatus := 200
	if len(status) > 0 {
		codeStatus = status[0]
	}
	return mcpJSONReply(store, codeStatus, JObj().
		Set("jsonrpc", JStr("2.0")).
		Set("id", id).
		Set("error", JObj().Set("code", JInt(int64(code))).Set("message", JStr(message))))
}

func mcpToolResult(payload *JVal) *JVal {
	return JObj().
		Set("content", JArr(JObj().Set("type", JStr("text")).Set("text", JStr(string(AppendJSON(nil, payload)))))).
		Set("structuredContent", payload).
		Set("isError", JBool(false))
}

func mcpToolError(message string) *JVal {
	return JObj().
		Set("content", JArr(JObj().Set("type", JStr("text")).Set("text", JStr(message)))).
		Set("isError", JBool(true))
}

// ---------------------------------------------------------------------------
// Arguments
// ---------------------------------------------------------------------------

// mcpArgError is the tools/call argument error: it becomes JSON-RPC
// -32602, never a transport 500.
type mcpArgError struct{ message string }

func (e *mcpArgError) Error() string { return e.message }

func mcpMissing(field string) error {
	return &mcpArgError{message: "Missing required argument: " + field}
}

func mcpRequiredString(args *JVal, field string) (string, error) {
	value, ok := args.Get(field).AsStr()
	if !ok || value == "" {
		return "", mcpMissing(field)
	}
	return value, nil
}

func mcpOptionalString(args *JVal, field, fallback string) string {
	if value, ok := args.Get(field).AsStr(); ok {
		return value
	}
	return fallback
}

func mcpOptionalBoolean(args *JVal, field string, fallback bool) bool {
	if value, ok := args.Get(field).AsBool(); ok {
		return value
	}
	return fallback
}

func mcpRequiredInt(args *JVal, field string) (float64, error) {
	value := args.Get(field)
	if value != nil && value.Kind == KNum {
		if i, ok := value.AsInt(); ok {
			return float64(i), nil
		}
	}
	if value != nil && value.Kind == KStr && isAllDigits(value.Str) {
		return parseDigitStringFloat(value.Str), nil
	}
	return 0, mcpMissing(field)
}

func isAllDigits(s string) bool {
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

// mcpRepoRef reads owner + repo from the arguments, accepting a full_name
// ("owner/repo") fallback when either part is absent.
func mcpRepoRef(args *JVal) (owner, name string, err error) {
	fullName, _ := args.Get("full_name").AsStr()
	if (!args.Has("owner") || !args.Has("repo")) && fullName != "" && strings.Contains(fullName, "/") {
		// JavaScript split('/', 2): the first two segments only.
		parts := strings.SplitN(fullName, "/", 3)
		if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
			return parts[0], parts[1], nil
		}
	}
	owner, err = mcpRequiredString(args, "owner")
	if err != nil {
		return "", "", err
	}
	name, err = mcpRequiredString(args, "repo")
	if err != nil {
		return "", "", err
	}
	return owner, name, nil
}

// ---------------------------------------------------------------------------
// tools/call
// ---------------------------------------------------------------------------

func mcpCallTool(store *Store, name string, args *JVal) (*JVal, error) {
	switch name {
	case "create_repo":
		repoName, err := mcpRequiredString(args, "name")
		if err != nil {
			return nil, err
		}
		repo, err := store.CreateRepo(repoName, mcpOptionalString(args, "description", ""), mcpOptionalBoolean(args, "private", false))
		if err != nil {
			var se *StoreError
			if errors.As(err, &se) {
				return mcpToolError(se.Message), nil
			}
			return nil, err
		}
		return mcpToolResult(repoObject(store, repo)), nil
	case "get_repo":
		owner, repoName, err := mcpRepoRef(args)
		if err != nil {
			return nil, err
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found"), nil
		}
		return mcpToolResult(repoObject(store, repo)), nil
	case "create_issue":
		owner, repoName, err := mcpRepoRef(args)
		if err != nil {
			return nil, err
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found"), nil
		}
		title, err := mcpRequiredString(args, "title")
		if err != nil {
			return nil, err
		}
		item, err := store.CreateItem(repo, KindIssue, title, mcpOptionalString(args, "body", ""), nil, "", "")
		if err != nil {
			return nil, err
		}
		return mcpToolResult(issueObject(store, repo, item)), nil
	case "list_issues":
		owner, repoName, err := mcpRepoRef(args)
		if err != nil {
			return nil, err
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found"), nil
		}
		state := mcpOptionalString(args, "state", "open")
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
		sort.Slice(items, func(i, j int) bool { return items[i].Number > items[j].Number })
		arr := JArr()
		for _, item := range items {
			arr.Arr = append(arr.Arr, issueObject(store, repo, item))
		}
		return mcpToolResult(arr), nil
	case "create_pull_request":
		owner, repoName, err := mcpRepoRef(args)
		if err != nil {
			return nil, err
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found"), nil
		}
		title, err := mcpRequiredString(args, "title")
		if err != nil {
			return nil, err
		}
		head, err := mcpRequiredString(args, "head")
		if err != nil {
			return nil, err
		}
		base, err := mcpRequiredString(args, "base")
		if err != nil {
			return nil, err
		}
		if _, found, err := store.ResolveRef(repo, head); err != nil {
			return nil, err
		} else if !found {
			return mcpToolError("Head branch " + head + " does not exist"), nil
		}
		if _, found, err := store.ResolveRef(repo, base); err != nil {
			return nil, err
		} else if !found {
			return mcpToolError("Base branch " + base + " does not exist"), nil
		}
		item, err := store.CreateItem(repo, KindPull, title, mcpOptionalString(args, "body", ""), nil, head, base)
		if err != nil {
			return nil, err
		}
		pull, err := pullObject(store, repo, item)
		if err != nil {
			return nil, err
		}
		return mcpToolResult(pull), nil
	case "merge_pull_request":
		owner, repoName, err := mcpRepoRef(args)
		if err != nil {
			return nil, err
		}
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError("Repository " + owner + "/" + repoName + " not found"), nil
		}
		number, err := mcpRequiredInt(args, "pull_number")
		if err != nil {
			return nil, err
		}
		var item *Item
		if number >= 1 && number <= 1<<30 {
			item = store.GetItem(repo, int(number))
		}
		numberText := formatJSNumber(number)
		if item == nil || item.Kind != KindPull {
			return mcpToolError("Pull request #" + numberText + " not found"), nil
		}
		if item.State != "open" {
			return mcpToolError("Pull request #" + numberText + " is already closed"), nil
		}
		sha, err := store.MergePullRequest(repo, item, item.Title+" (#"+formatJSNumber(float64(item.Number))+")")
		if err != nil {
			return nil, err
		}
		if sha == "" {
			return mcpToolError("Pull request #" + numberText + " cannot be merged due to conflicts"), nil
		}
		return mcpToolResult(JObj().
			Set("sha", JStr(sha)).
			Set("merged", JBool(true)).
			Set("message", JStr("Pull request successfully merged"))), nil
	default:
		return nil, &mcpArgError{message: "Unknown tool: " + name}
	}
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

// handleMcp answers one POST /mcp body, given as raw text so a body that is
// not JSON can answer the MCP parse error (HTTP 400, JSON-RPC -32700)
// instead of the generic JSON 400.
func handleMcp(store *Store, rawBody string) (handlerReply, error) {
	parsed, err := ParseJSON([]byte(rawBody))
	if err != nil {
		return mcpError(store, JNul(), mcpParseError, "Parse error", 400), nil
	}
	if parsed == nil || parsed.Kind != KObj {
		return mcpError(store, JNul(), mcpInvalidRequest, "Invalid Request"), nil
	}
	// Notifications (no id) are accepted silently: 202, no body.
	if !parsed.Has("id") {
		return handlerReply{
			status:  202,
			headers: [][2]string{{"x-github-request-id", store.NextRequestID()}},
			body:    nil,
		}, nil
	}
	id := parsed.Get("id")
	method, ok := parsed.Get("method").AsStr()
	if !ok || method == "" {
		return mcpError(store, id, mcpInvalidRequest, "Invalid Request"), nil
	}

	switch method {
	case "server/discover":
		versions := JArr()
		for _, v := range mcpSupportedVersions {
			versions.Arr = append(versions.Arr, JStr(v))
		}
		serverInfo := JObj().Set("name", JStr(mcpServerName)).Set("version", JStr(mcpServerVersion))
		return mcpResult(store, id, JObj().
			Set("supportedVersions", versions).
			Set("capabilities", JObj().Set("tools", JObj())).
			Set("serverInfo", serverInfo).
			Set("_meta", JObj().Set("io.modelcontextprotocol/serverInfo", JObj().Set("name", JStr(mcpServerName)).Set("version", JStr(mcpServerVersion))))), nil
	case "initialize":
		params := parsed.Get("params")
		offered, _ := params.Get("protocolVersion").AsStr()
		chosen := "2025-11-25"
		for _, v := range mcpSupportedVersions {
			if offered != "" && offered == v {
				chosen = offered
				break
			}
		}
		return mcpResult(store, id, JObj().
			Set("protocolVersion", JStr(chosen)).
			Set("capabilities", JObj().Set("tools", JObj())).
			Set("serverInfo", JObj().Set("name", JStr(mcpServerName)).Set("version", JStr(mcpServerVersion)))), nil
	case "ping":
		return mcpResult(store, id, JObj()), nil
	case "tools/list":
		return mcpResult(store, id, mcpToolsListResult), nil
	case "tools/call":
		params := parsed.Get("params")
		if params == nil || params.Kind != KObj {
			return mcpError(store, id, mcpInvalidParams, "tools/call requires a tool name"), nil
		}
		toolName, ok := params.Get("name").AsStr()
		if !ok {
			return mcpError(store, id, mcpInvalidParams, "tools/call requires a tool name"), nil
		}
		args := params.Get("arguments")
		if args == nil || args.Kind != KObj {
			args = JObj()
		}
		result, err := mcpCallTool(store, toolName, args)
		if err != nil {
			var invalid *mcpArgError
			if errors.As(err, &invalid) {
				return mcpError(store, id, mcpInvalidParams, invalid.message), nil
			}
			return handlerReply{}, err
		}
		return mcpResult(store, id, result), nil
	default:
		return mcpError(store, id, mcpMethodNotFound, "Method not found: "+method), nil
	}
}
