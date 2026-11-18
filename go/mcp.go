// Model Context Protocol at /mcp: JSON-RPC 2.0 over Streamable HTTP, answering application/json.
// Speaks 2026-07-28 (server/discover) and the handshake revisions through initialize. The tools act
// on the same store as REST, through the REST handlers themselves.
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

var serverInfo = J{"name": "forge-mcp", "title": "Forge MCP", "version": "2.0.0"}
var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema J      `json:"inputSchema"`
	required    []string
	run         func(a J, base string) any
}

func sProp(d string) J { return J{"type": "string", "description": d} }
func nProp(d string) J { return J{"type": "number", "description": d} }
func bProp(d string) J { return J{"type": "boolean", "description": d} }

func schema(props J, required ...string) J {
	if required == nil {
		required = []string{}
	}
	return J{"type": "object", "properties": props, "required": required}
}

// restCall runs a REST handler in process; a non-2xx answer becomes a tool error.
func restCall(method, path, base string, body any, query url.Values) any {
	if query == nil {
		query = url.Values{}
	}
	if body != nil {
		// Through JSON, as a REST client would send it: numbers become float64 and nils drop out.
		raw, _ := json.Marshal(body)
		json.Unmarshal(raw, &body)
	}
	reply := handleRest(&restReq{method: method, path: path, query: query, body: body, base: base})
	var out any
	if reply.raw != nil {
		json.Unmarshal(reply.raw, &out)
	} else {
		out = reply.body
	}
	if reply.status >= 300 {
		msg := ""
		if m, ok := reply.body.(J); ok {
			msg, _ = m["message"].(string)
		}
		panic(&httpError{status: reply.status, msg: msg})
	}
	return out
}

func seg(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return url.PathEscape(x)
	}
	return url.PathEscape(fmt.Sprint(v))
}

func repoPath(a J) string { return "/repos/" + seg(a["owner"]) + "/" + seg(a["repo"]) }

func filePath(v any) string {
	s, _ := v.(string)
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func qv(pairs ...string) url.Values {
	q := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		q.Set(pairs[i], pairs[i+1])
	}
	return q
}

func orDefault(v any, def string) string {
	switch x := v.(type) {
	case string:
		if x != "" {
			return x
		}
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return def
}

func omit(m J) J {
	out := J{}
	for k, v := range m {
		if v != nil {
			out[k] = v
		}
	}
	return out
}

var tools = []*mcpTool{
	{Name: "create_repo", Description: "Create a new repository owned by the authenticated user.",
		InputSchema: schema(J{"name": sProp("Repository name"), "description": sProp("Repository description"), "private": bProp("Whether the repository is private"), "autoInit": bProp("Initialize with a README")}, "name"),
		required:    []string{"name"},
		run: func(a J, base string) any {
			return restCall("POST", "/user/repos", base, omit(J{"name": a["name"], "description": a["description"], "private": a["private"] == true, "auto_init": a["autoInit"] == true}), nil)
		}},
	{Name: "get_repo", Description: "Get a repository by owner and name.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name")}, "owner", "repo"), required: []string{"owner", "repo"},
		run: func(a J, base string) any { return restCall("GET", repoPath(a), base, nil, nil) }},
	{Name: "list_repos", Description: "List the repositories of the authenticated user.",
		InputSchema: schema(J{"page": nProp("Page number"), "perPage": nProp("Results per page")}),
		run: func(a J, base string) any {
			return restCall("GET", "/user/repos", base, nil, qv("page", orDefault(a["page"], "1"), "per_page", orDefault(a["perPage"], "30")))
		}},
	{Name: "create_issue", Description: "Create a new issue in a repository.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "title": sProp("Issue title"), "body": sProp("Issue body"),
			"labels": J{"type": "array", "items": J{"type": "string"}, "description": "Labels to apply"}}, "owner", "repo", "title"),
		required: []string{"owner", "repo", "title"},
		run: func(a J, base string) any {
			return restCall("POST", repoPath(a)+"/issues", base, omit(J{"title": a["title"], "body": a["body"], "labels": a["labels"]}), nil)
		}},
	{Name: "get_issue", Description: "Get one issue of a repository by number.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "issue_number": nProp("Issue number")}, "owner", "repo", "issue_number"),
		required:    []string{"owner", "repo", "issue_number"},
		run: func(a J, base string) any {
			return restCall("GET", repoPath(a)+"/issues/"+seg(a["issue_number"]), base, nil, nil)
		}},
	{Name: "list_issues", Description: "List the issues of a repository, filtered by state.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "state": J{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "Issue state filter"},
			"page": nProp("Page number"), "perPage": nProp("Results per page")}, "owner", "repo"),
		required: []string{"owner", "repo"},
		run: func(a J, base string) any {
			return restCall("GET", repoPath(a)+"/issues", base, nil, qv("state", strings.ToLower(orDefault(a["state"], "open")), "page", orDefault(a["page"], "1"), "per_page", orDefault(a["perPage"], "100")))
		}},
	{Name: "update_issue", Description: "Update an issue: its title, body or state.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "issue_number": nProp("Issue number"), "title": sProp("New title"), "body": sProp("New body"),
			"state": J{"type": "string", "enum": []string{"open", "closed"}, "description": "New state"}}, "owner", "repo", "issue_number"),
		required: []string{"owner", "repo", "issue_number"},
		run: func(a J, base string) any {
			return restCall("PATCH", repoPath(a)+"/issues/"+seg(a["issue_number"]), base, omit(J{"title": a["title"], "body": a["body"], "state": a["state"]}), nil)
		}},
	{Name: "add_issue_comment", Description: "Add a comment to an issue.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "issue_number": nProp("Issue number"), "body": sProp("Comment text")}, "owner", "repo", "issue_number", "body"),
		required:    []string{"owner", "repo", "issue_number", "body"},
		run: func(a J, base string) any {
			return restCall("POST", repoPath(a)+"/issues/"+seg(a["issue_number"])+"/comments", base, J{"body": a["body"]}, nil)
		}},
	{Name: "create_pull_request", Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "title": sProp("Pull request title"), "body": sProp("Pull request description"),
			"head": sProp("Branch with the changes"), "base": sProp("Branch to merge into")}, "owner", "repo", "title", "head", "base"),
		required: []string{"owner", "repo", "title", "head", "base"},
		run: func(a J, base string) any {
			return restCall("POST", repoPath(a)+"/pulls", base, omit(J{"title": a["title"], "body": a["body"], "head": a["head"], "base": a["base"]}), nil)
		}},
	{Name: "get_pull_request", Description: "Get one pull request by number.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "pull_number": nProp("Pull request number")}, "owner", "repo", "pull_number"),
		required:    []string{"owner", "repo", "pull_number"},
		run: func(a J, base string) any {
			return restCall("GET", repoPath(a)+"/pulls/"+seg(a["pull_number"]), base, nil, nil)
		}},
	{Name: "list_pull_requests", Description: "List the pull requests of a repository, filtered by state.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "state": J{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "Pull request state filter"}}, "owner", "repo"),
		required:    []string{"owner", "repo"},
		run: func(a J, base string) any {
			return restCall("GET", repoPath(a)+"/pulls", base, nil, qv("state", orDefault(a["state"], "open"), "per_page", "100"))
		}},
	{Name: "merge_pull_request", Description: "Merge a pull request (merge, squash or rebase).",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "pull_number": nProp("Pull request number"),
			"merge_method": J{"type": "string", "enum": []string{"merge", "squash", "rebase"}, "description": "Merge method"}, "commit_title": sProp("Title of the merge commit"),
			"commit_message": sProp("Body of the merge commit")}, "owner", "repo", "pull_number"),
		required: []string{"owner", "repo", "pull_number"},
		run: func(a J, base string) any {
			return restCall("PUT", repoPath(a)+"/pulls/"+seg(a["pull_number"])+"/merge", base,
				omit(J{"merge_method": orDefault(a["merge_method"], "squash"), "commit_title": a["commit_title"], "commit_message": a["commit_message"]}), nil)
		}},
	{Name: "list_branches", Description: "List the branches of a repository.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name")}, "owner", "repo"), required: []string{"owner", "repo"},
		run: func(a J, base string) any {
			return restCall("GET", repoPath(a)+"/branches", base, nil, qv("per_page", "100"))
		}},
	{Name: "create_branch", Description: "Create a branch from another branch (the default branch when none is named).",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "branch": sProp("New branch name"), "from_branch": sProp("Source branch")}, "owner", "repo", "branch"),
		required:    []string{"owner", "repo", "branch"},
		run: func(a J, base string) any {
			repo := restCall("GET", repoPath(a), base, nil, nil).(J)
			from := orDefault(a["from_branch"], fmt.Sprint(repo["default_branch"]))
			branch := restCall("GET", repoPath(a)+"/branches/"+url.PathEscape(from), base, nil, nil).(J)
			sha := branch["commit"].(J)["sha"]
			return restCall("POST", repoPath(a)+"/git/refs", base, J{"ref": "refs/heads/" + fmt.Sprint(a["branch"]), "sha": sha}, nil)
		}},
	{Name: "get_file_contents", Description: "Get the contents of a file or directory at a ref.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "path": sProp("Path in the repository"), "ref": sProp("Branch, tag or commit")}, "owner", "repo", "path"),
		required:    []string{"owner", "repo", "path"},
		run: func(a J, base string) any {
			var q url.Values
			if ref, ok := a["ref"].(string); ok {
				q = qv("ref", ref)
			}
			return restCall("GET", repoPath(a)+"/contents/"+filePath(a["path"]), base, nil, q)
		}},
	{Name: "create_or_update_file", Description: "Create or update one file with a commit on a branch.",
		InputSchema: schema(J{"owner": sProp("Repository owner"), "repo": sProp("Repository name"), "path": sProp("Path in the repository"), "content": sProp("File content (plain text)"),
			"message": sProp("Commit message"), "branch": sProp("Branch to commit to"), "sha": sProp("Blob sha of the file being replaced")}, "owner", "repo", "path", "content", "message"),
		required: []string{"owner", "repo", "path", "content", "message"},
		run: func(a J, base string) any {
			content, _ := a["content"].(string)
			return restCall("PUT", repoPath(a)+"/contents/"+filePath(a["path"]), base,
				omit(J{"message": a["message"], "content": base64.StdEncoding.EncodeToString([]byte(content)), "branch": a["branch"], "sha": a["sha"]}), nil)
		}},
}

var toolByName = func() map[string]*mcpTool {
	m := map[string]*mcpTool{}
	for _, t := range tools {
		m[t.Name] = t
	}
	return m
}()

// toolList is marshalled once, so every tools/list answers the same bytes.
var toolList = func() json.RawMessage {
	raw, _ := json.Marshal(J{"tools": tools})
	return raw
}()

func rpcError(id any, code int, msg string) J {
	return J{"jsonrpc": "2.0", "id": id, "error": J{"code": code, "message": msg}}
}

func rpcResult(id any, result any) J { return J{"jsonrpc": "2.0", "id": id, "result": result} }

func toolText(v any, isError bool) J {
	text, isStr := v.(string)
	if !isStr {
		raw, _ := json.Marshal(v)
		text = string(raw)
	}
	return J{"content": []J{{"type": "text", "text": text}}, "isError": isError}
}

func runTool(params J, id any, base string) (out J) {
	name, ok := params["name"].(string)
	if !ok {
		return rpcError(id, -32602, "Invalid params: tools/call needs a tool name")
	}
	t := toolByName[name]
	if t == nil {
		return rpcError(id, -32602, "Unknown tool: "+name)
	}
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = J{}
	}
	var missing []string
	for _, k := range t.required {
		if v, has := args[k]; !has || v == nil || v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return rpcResult(id, toolText("missing required argument(s): "+strings.Join(missing, ", "), true))
	}
	defer func() {
		if rec := recover(); rec != nil {
			if he, ok := rec.(*httpError); ok {
				out = rpcResult(id, toolText(strconv.Itoa(he.status)+": "+he.msg, true))
				return
			}
			out = rpcResult(id, toolText(fmt.Sprintf("tool failed: %v", rec), true))
		}
	}()
	return rpcResult(id, toolText(t.run(args, base), false))
}

func dispatch(msg any, base string) J {
	m, ok := msg.(map[string]any)
	if !ok {
		return rpcError(nil, -32600, "Invalid Request")
	}
	_, hasID := m["id"]
	id := m["id"]
	switch id.(type) {
	case string, float64:
	default:
		id = nil
	}
	method, _ := m["method"].(string)
	if m["jsonrpc"] != "2.0" || method == "" {
		if !hasID {
			return nil
		}
		return rpcError(id, -32600, "Invalid Request")
	}
	if !hasID {
		return nil
	}
	params, _ := m["params"].(map[string]any)
	if params == nil {
		params = J{}
	}
	switch method {
	case "server/discover":
		return rpcResult(id, J{"supportedVersions": supportedVersions, "capabilities": J{"tools": J{"listChanged": false}}, "serverInfo": serverInfo,
			"instructions": "A GitHub-compatible forge: repositories, issues and pull requests.", "_meta": J{"io.modelcontextprotocol/serverInfo": serverInfo}})
	case "initialize":
		asked, _ := params["protocolVersion"].(string)
		version := "2025-11-25"
		if contains(supportedVersions, asked) {
			version = asked
		}
		return rpcResult(id, J{"protocolVersion": version, "capabilities": J{"tools": J{"listChanged": false}}, "serverInfo": serverInfo})
	case "ping":
		return rpcResult(id, J{})
	case "tools/list":
		return rpcResult(id, toolList)
	case "tools/call":
		return runTool(params, id, base)
	case "resources/list":
		return rpcResult(id, J{"resources": []any{}})
	case "prompts/list":
		return rpcResult(id, J{"prompts": []any{}})
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

// handleMcp answers (status, body or nil, extra headers).
func handleMcp(method string, authed bool, raw []byte, base string) (int, any, map[string]string) {
	if !authed {
		return 401, rpcError(nil, -32001, "Unauthorized: a Bearer token is required"), map[string]string{"WWW-Authenticate": `Bearer realm="forge"`}
	}
	switch method {
	case "GET":
		return 405, rpcError(nil, -32000, "This server opens no GET stream"), map[string]string{"Allow": "POST, DELETE"}
	case "DELETE":
		return 204, nil, nil
	case "POST":
	default:
		return 405, rpcError(nil, -32000, "Method not allowed"), map[string]string{"Allow": "POST"}
	}
	var msg any
	if err := json.Unmarshal(raw, &msg); err != nil {
		return 400, rpcError(nil, -32700, "Parse error"), nil
	}
	if list, ok := msg.([]any); ok {
		var answers []any
		for _, e := range list {
			if a := dispatch(e, base); a != nil {
				answers = append(answers, a)
			}
		}
		if len(answers) == 0 {
			return 202, nil, nil
		}
		return 200, answers, nil
	}
	if a := dispatch(msg, base); a != nil {
		return 200, a, nil
	}
	return 202, nil, nil
}
