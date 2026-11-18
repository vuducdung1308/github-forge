// GitHub REST v3 under /api/v3: repositories, branches, refs, commits, contents, labels, issues,
// comments and pull requests. Every answer is JSON; errors carry {message, documentation_url}.
package main

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type J = map[string]any

type restReq struct {
	method string
	path   string
	query  url.Values
	body   any
	base   string
}

type restReply struct {
	status  int
	body    any    // marshalled unless raw is set
	raw     []byte // pre-serialized JSON
	headers map[string]string
}

type httpError struct {
	status int
	msg    string
	extra  J
}

func (e *httpError) Error() string { return e.msg }

func fail(status int, msg string) { panic(&httpError{status: status, msg: msg}) }
func failExtra(status int, msg string, extra J) {
	panic(&httpError{status: status, msg: msg, extra: extra})
}
func notFound() { fail(404, "Not Found") }
func invalid(msg string, errs ...J) {
	if len(errs) > 0 {
		list := make([]any, len(errs))
		for i, e := range errs {
			list[i] = e
		}
		failExtra(422, msg, J{"errors": list})
	}
	fail(422, msg)
}

func errorBody(status int, msg string, extra J) J {
	out := J{"message": msg, "documentation_url": "https://docs.github.com/rest", "status": strconv.Itoa(status)}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func obj(v any) J {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return J{}
}

func str(m J, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok
}

func ok(body any) restReply { return restReply{status: 200, body: body} }

// ------------------------------------------------------------------ serializers

func actorJSON(u *User) J {
	return J{"login": u.Login, "id": u.ID, "node_id": globalIDOf(u), "type": "User", "site_admin": false}
}

func userJSON(u *User, base string) J {
	return J{"login": u.Login, "id": u.ID, "node_id": globalIDOf(u), "avatar_url": base + "/avatars/" + u.Login, "url": base + "/api/v3/users/" + u.Login,
		"html_url": base + "/" + u.Login, "type": "User", "site_admin": false, "name": u.Name, "email": u.Email}
}

func repoJSON(r *Repo, base string) J {
	api := base + "/api/v3/repos/" + r.FullName()
	open := 0
	for _, is := range r.Issues {
		if is.State == "open" {
			open++
		}
	}
	vis := "public"
	if r.Private {
		vis = "private"
	}
	return J{"id": r.ID, "node_id": globalIDOf(r), "name": r.Name, "full_name": r.FullName(), "private": r.Private, "visibility": vis,
		"owner": userJSON(r.Owner, base), "html_url": base + "/" + r.FullName(), "description": r.Description, "fork": false, "url": api,
		"clone_url": base + "/" + r.FullName() + ".git", "git_url": base + "/" + r.FullName() + ".git", "created_at": r.CreatedAt,
		"updated_at": r.UpdatedAt, "pushed_at": r.PushedAt, "size": 0, "language": nil, "default_branch": r.DefaultBranch,
		"has_issues": true, "has_projects": false, "has_wiki": false, "archived": false, "disabled": false,
		"open_issues_count": open, "open_issues": open, "forks_count": 0, "stargazers_count": 0, "watchers_count": 0,
		"permissions": J{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true}}
}

func labelJSON(l *Label, r *Repo, base string) J {
	return J{"id": l.ID, "node_id": globalIDOf(l), "url": base + "/api/v3/repos/" + r.FullName() + "/labels/" + url.PathEscape(l.Name),
		"name": l.Name, "color": l.Color, "description": l.Description, "default": false}
}

func labelsJSON(ls []*Label, r *Repo, base string) []J {
	out := make([]J, 0, len(ls))
	for _, l := range ls {
		out = append(out, labelJSON(l, r, base))
	}
	return out
}

// issueBody is the hot read and write path's object: a struct keeps field order and marshals fast.
type issueBody struct {
	ID                int     `json:"id"`
	NodeID            string  `json:"node_id"`
	URL               string  `json:"url"`
	RepositoryURL     string  `json:"repository_url"`
	HTMLURL           string  `json:"html_url"`
	Number            int     `json:"number"`
	Title             string  `json:"title"`
	Body              string  `json:"body"`
	State             string  `json:"state"`
	StateReason       *string `json:"state_reason"`
	Locked            bool    `json:"locked"`
	User              J       `json:"user"`
	Labels            []J     `json:"labels"`
	Assignee          any     `json:"assignee"`
	Assignees         []any   `json:"assignees"`
	Comments          int     `json:"comments"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	ClosedAt          *string `json:"closed_at"`
	AuthorAssociation string  `json:"author_association"`
}

func issueJSON(is *Issue, base string) []byte {
	if is.json != nil {
		return is.json
	}
	r := is.Repo
	api := base + "/api/v3/repos/" + r.FullName()
	n := strconv.Itoa(is.Number)
	b, _ := json.Marshal(issueBody{ID: is.ID, NodeID: globalIDOf(is), URL: api + "/issues/" + n, RepositoryURL: api, HTMLURL: base + "/" + r.FullName() + "/issues/" + n,
		Number: is.Number, Title: is.Title, Body: is.Body, State: is.State, StateReason: is.StateReason, User: actorJSON(viewer),
		Labels: labelsJSON(is.Labels, r, base), Assignees: []any{}, Comments: len(is.Comments), CreatedAt: is.CreatedAt,
		UpdatedAt: is.UpdatedAt, ClosedAt: is.ClosedAt, AuthorAssociation: "OWNER"})
	is.json = b
	return b
}

func commentJSON(c *Comment, base string) J {
	full := c.Repo.FullName()
	return J{"id": c.ID, "node_id": globalIDOf(c), "url": base + "/api/v3/repos/" + full + "/issues/comments/" + strconv.Itoa(c.ID),
		"html_url":  base + "/" + full + "/issues/" + strconv.Itoa(c.Number) + "#issuecomment-" + strconv.Itoa(c.ID),
		"issue_url": base + "/api/v3/repos/" + full + "/issues/" + strconv.Itoa(c.Number), "body": c.Body, "user": actorJSON(viewer),
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "author_association": "OWNER"}
}

func pullJSON(p *Pull, base string, detail bool) J {
	r := p.Repo
	api := base + "/api/v3/repos/" + r.FullName()
	n := strconv.Itoa(p.Number)
	side := func(ref, sha string) J {
		return J{"label": r.Owner.Login + ":" + ref, "ref": ref, "sha": sha, "user": actorJSON(viewer), "repo": repoJSON(r, base)}
	}
	out := J{"id": p.ID, "node_id": globalIDOf(p), "url": api + "/pulls/" + n, "html_url": base + "/" + r.FullName() + "/pull/" + n,
		"diff_url": base + "/" + r.FullName() + "/pull/" + n + ".diff", "issue_url": api + "/issues/" + n, "number": p.Number,
		"state": p.State, "locked": false, "title": p.Title, "body": p.Body, "user": actorJSON(viewer), "labels": []any{},
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "closed_at": p.ClosedAt, "merged_at": p.MergedAt,
		"merge_commit_sha": p.MergeCommitSHA, "draft": false, "head": side(p.Head, pullHeadSHA(p)), "base": side(p.Base, pullBaseSHA(p)),
		"author_association": "OWNER", "merged": p.Merged}
	if detail {
		files := pullFiles(p)
		m := pullMergeable(p)
		adds, dels := 0, 0
		for _, f := range files {
			adds += f.Additions
			dels += f.Deletions
		}
		out["mergeable"] = m
		state := "dirty"
		if p.Merged {
			state = "unknown"
		} else if m != nil && *m {
			state = "clean"
		}
		out["mergeable_state"] = state
		if p.Merged {
			out["merged_by"] = userJSON(viewer, base)
		} else {
			out["merged_by"] = nil
		}
		out["comments"] = len(p.Comments)
		out["commits"] = 1
		out["additions"] = adds
		out["deletions"] = dels
		out["changed_files"] = len(files)
	}
	return out
}

func commitJSON(r *Repo, sha, base string) J {
	c := r.commit(sha)
	if c == nil {
		return nil
	}
	api := base + "/api/v3/repos/" + r.FullName()
	parents := make([]J, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, J{"sha": p, "url": api + "/commits/" + p})
	}
	return J{"sha": sha, "node_id": "C_" + sha, "url": api + "/commits/" + sha, "html_url": base + "/" + r.FullName() + "/commit/" + sha,
		"commit": J{"message": strings.TrimSuffix(c.Message, "\n"), "author": parseIdent(c.Author), "committer": parseIdent(c.Committer),
			"tree": J{"sha": c.Tree, "url": api + "/git/trees/" + c.Tree}, "url": api + "/git/commits/" + sha},
		"author": nil, "committer": nil, "parents": parents}
}

func commitSummary(r *Repo, sha, base string) J {
	c := r.commit(sha)
	api := base + "/api/v3/repos/" + r.FullName()
	parents := make([]J, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, J{"sha": p})
	}
	return J{"sha": sha, "node_id": "C_" + sha, "url": api + "/git/commits/" + sha, "html_url": base + "/" + r.FullName() + "/commit/" + sha,
		"message": strings.TrimSuffix(c.Message, "\n"), "author": parseIdent(c.Author), "committer": parseIdent(c.Committer),
		"tree": J{"sha": c.Tree}, "parents": parents}
}

// ------------------------------------------------------------------ pagination

func pageArgs(q url.Values) (int, int) {
	per, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || per < 1 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	return per, page
}

// paginate answers the [lo, hi) window and the Link header for a list of n items.
func paginate(req *restReq, n int, path string) (int, int, map[string]string) {
	per, page := pageArgs(req.query)
	lo := (page - 1) * per
	if lo > n {
		lo = n
	}
	hi := lo + per
	if hi > n {
		hi = n
	}
	last := int(math.Max(1, math.Ceil(float64(n)/float64(per))))
	link := func(target int, rel string) string {
		q := url.Values{}
		for k, v := range req.query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(per))
		q.Set("page", strconv.Itoa(target))
		return "<" + req.base + "/api/v3" + path + "?" + q.Encode() + `>; rel="` + rel + `"`
	}
	var links []string
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"), link(page-1, "prev"))
	}
	if len(links) == 0 {
		return lo, hi, nil
	}
	return lo, hi, map[string]string{"Link": strings.Join(links, ", ")}
}

// ------------------------------------------------------------------ routing

type handler func(req *restReq, p []string) restReply

type restRoute struct {
	segs    []string // "" for a parameter
	tail    bool     // the last parameter takes the rest of the path
	handler handler
}

var routes = map[string][]restRoute{}

func addRoute(method, pattern string, h handler) {
	var segs []string
	tail := false
	for _, s := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if strings.HasPrefix(s, "{") {
			if strings.HasSuffix(s, "*}") {
				tail = true
			}
			segs = append(segs, "")
		} else {
			segs = append(segs, s)
		}
	}
	if pattern == "" || pattern == "/" {
		segs = []string{}
	}
	routes[method] = append(routes[method], restRoute{segs, tail, h})
}

func (rt restRoute) match(parts []string) ([]string, bool) {
	if rt.tail {
		if len(parts) < len(rt.segs) {
			return nil, false
		}
	} else if len(parts) != len(rt.segs) {
		return nil, false
	}
	var params []string
	for i, s := range rt.segs {
		if s == "" {
			if rt.tail && i == len(rt.segs)-1 {
				params = append(params, unescape(strings.Join(parts[i:], "/")))
				return params, true
			}
			params = append(params, unescape(parts[i]))
		} else if parts[i] != s {
			return nil, false
		}
	}
	return params, true
}

func unescape(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

func handleRest(req *restReq) (reply restReply) {
	defer func() {
		if rec := recover(); rec != nil {
			if he, ok := rec.(*httpError); ok {
				reply = restReply{status: he.status, body: errorBody(he.status, he.msg, he.extra)}
				return
			}
			panic(rec)
		}
	}()
	path := strings.TrimRight(req.path, "/")
	var parts []string
	if path != "" {
		parts = strings.Split(path[1:], "/")
	}
	for _, rt := range routes[req.method] {
		if params, ok := rt.match(parts); ok {
			return rt.handler(req, params)
		}
	}
	notFound()
	return
}

func repoOf(p []string) *Repo {
	r := findRepo(p[0], p[1])
	if r == nil {
		notFound()
	}
	return r
}

func rootJSON(base string) J {
	return J{"current_user_url": base + "/api/v3/user", "repository_url": base + "/api/v3/repos/{owner}/{repo}",
		"user_repositories_url": base + "/api/v3/user/repos", "issues_url": base + "/api/v3/repos/{owner}/{repo}/issues/{number}",
		"rate_limit_url": base + "/api/v3/rate_limit"}
}

func init() {
	// The two routes the benchmark hits thousands of times are registered first.
	addRoute("GET", "/repos/{owner}/{repo}/issues/{number}", getIssue)
	addRoute("POST", "/repos/{owner}/{repo}/issues", postIssue)

	addRoute("GET", "/", func(req *restReq, _ []string) restReply { return ok(rootJSON(req.base)) })
	addRoute("GET", "/meta", func(*restReq, []string) restReply {
		return ok(J{"installed_version": "3.15.0", "verifiable_password_authentication": false})
	})
	addRoute("GET", "/rate_limit", func(*restReq, []string) restReply {
		core := J{"limit": 5000, "remaining": 5000, "reset": 0, "used": 0}
		return ok(J{"resources": J{"core": core}, "rate": core})
	})
	addRoute("GET", "/user", func(req *restReq, _ []string) restReply { return ok(userJSON(viewer, req.base)) })
	addRoute("GET", "/users/{user}", func(req *restReq, p []string) restReply {
		if !strings.EqualFold(p[0], viewer.Login) {
			notFound()
		}
		return ok(userJSON(viewer, req.base))
	})

	// ---- repositories
	listOf := func(req *restReq, list []*Repo, path string) restReply {
		lo, hi, h := paginate(req, len(list), path)
		out := make([]J, 0, hi-lo)
		for _, r := range list[lo:hi] {
			out = append(out, repoJSON(r, req.base))
		}
		return restReply{status: 200, body: out, headers: h}
	}
	addRoute("GET", "/user/repos", func(req *restReq, _ []string) restReply { return listOf(req, listRepos(), "/user/repos") })
	addRoute("GET", "/users/{user}/repos", func(req *restReq, p []string) restReply {
		var mine []*Repo
		for _, r := range listRepos() {
			if strings.EqualFold(r.Owner.Login, p[0]) {
				mine = append(mine, r)
			}
		}
		return listOf(req, mine, "/users/"+p[0]+"/repos")
	})
	addRoute("POST", "/user/repos", func(req *restReq, _ []string) restReply {
		b := obj(req.body)
		name, _ := str(b, "name")
		if !validRepoName(name) {
			invalid("Validation Failed", J{"resource": "Repository", "field": "name", "code": "invalid"})
		}
		var desc *string
		if d, has := str(b, "description"); has {
			desc = &d
		}
		private := b["private"] == true || b["visibility"] == "private"
		r := createRepo(name, desc, private)
		if r == nil {
			invalid("Repository creation failed.", J{"resource": "Repository", "field": "name", "code": "custom", "message": "name already exists on this account"})
		}
		if b["auto_init"] == true {
			commitFile(r, r.DefaultBranch, "README.md", []byte("# "+r.Name+"\n"), "Initial commit")
		}
		return restReply{status: 201, body: repoJSON(r, req.base)}
	})
	addRoute("GET", "/repos/{owner}/{repo}", func(req *restReq, p []string) restReply { return ok(repoJSON(repoOf(p), req.base)) })
	addRoute("PATCH", "/repos/{owner}/{repo}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		b := obj(req.body)
		if v, has := b["description"]; has {
			if s, isStr := v.(string); isStr {
				r.Description = &s
			} else if v == nil {
				r.Description = nil
			}
		}
		if v, isBool := b["private"].(bool); isBool {
			r.Private = v
		}
		if v, isStr := b["default_branch"].(string); isStr && v != "" {
			r.DefaultBranch = v
		}
		r.touch()
		return ok(repoJSON(r, req.base))
	})
	addRoute("DELETE", "/repos/{owner}/{repo}", func(req *restReq, p []string) restReply {
		deleteRepo(repoOf(p))
		return restReply{status: 204}
	})

	// ---- branches and refs
	branchJSON := func(r *Repo, name, sha, base string) J {
		return J{"name": name, "commit": J{"sha": sha, "url": base + "/api/v3/repos/" + r.FullName() + "/commits/" + sha}, "protected": false}
	}
	addRoute("GET", "/repos/{owner}/{repo}/branches", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		var names []string
		for _, n := range r.sortedRefs() {
			if strings.HasPrefix(n, "refs/heads/") {
				names = append(names, n)
			}
		}
		lo, hi, h := paginate(req, len(names), "/repos/"+r.FullName()+"/branches")
		out := make([]J, 0, hi-lo)
		for _, n := range names[lo:hi] {
			out = append(out, branchJSON(r, n[11:], r.Refs[n], req.base))
		}
		return restReply{status: 200, body: out, headers: h}
	})
	addRoute("GET", "/repos/{owner}/{repo}/branches/{branch*}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		sha, has := r.branchTip(p[2])
		if !has {
			fail(404, "Branch not found")
		}
		return ok(branchJSON(r, p[2], sha, req.base))
	})
	refJSON := func(r *Repo, ref, sha, base string) J {
		typ := "commit"
		if o := r.Objects[sha]; o != nil {
			typ = o.Type
		}
		return J{"ref": ref, "node_id": "REF_" + base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(r.ID)+":"+ref)),
			"url":    base + "/api/v3/repos/" + r.FullName() + "/git/" + ref,
			"object": J{"sha": sha, "type": typ, "url": base + "/api/v3/repos/" + r.FullName() + "/git/" + typ + "s/" + sha}}
	}
	addRoute("GET", "/repos/{owner}/{repo}/git/refs", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		out := []J{}
		for _, n := range r.sortedRefs() {
			out = append(out, refJSON(r, n, r.Refs[n], req.base))
		}
		return ok(out)
	})
	for _, pattern := range []string{"/repos/{owner}/{repo}/git/refs/{ref*}", "/repos/{owner}/{repo}/git/ref/{ref*}"} {
		addRoute("GET", pattern, func(req *restReq, p []string) restReply {
			r := repoOf(p)
			ref := "refs/" + p[2]
			sha, has := r.Refs[ref]
			if !has {
				notFound()
			}
			return ok(refJSON(r, ref, sha, req.base))
		})
	}
	addRoute("POST", "/repos/{owner}/{repo}/git/refs", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		b := obj(req.body)
		ref, _ := str(b, "ref")
		sha, _ := str(b, "sha")
		if !strings.HasPrefix(ref, "refs/") || len(strings.Split(ref, "/")) < 3 {
			invalid("Reference name is invalid")
		}
		if !isOID(sha) || r.Objects[sha] == nil {
			invalid("Object does not exist")
		}
		if _, has := r.Refs[ref]; has {
			invalid("Reference already exists")
		}
		r.setRef(ref, sha)
		return restReply{status: 201, body: refJSON(r, ref, sha, req.base)}
	})
	addRoute("PATCH", "/repos/{owner}/{repo}/git/refs/{ref*}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		ref := "refs/" + p[2]
		if _, has := r.Refs[ref]; !has {
			notFound()
		}
		sha, _ := str(obj(req.body), "sha")
		if r.Objects[sha] == nil {
			invalid("Object does not exist")
		}
		r.setRef(ref, sha)
		return ok(refJSON(r, ref, sha, req.base))
	})
	addRoute("DELETE", "/repos/{owner}/{repo}/git/refs/{ref*}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		ref := "refs/" + p[2]
		if _, has := r.Refs[ref]; !has {
			invalid("Reference does not exist")
		}
		r.setRef(ref, "")
		return restReply{status: 204}
	})

	// ---- commits and git data
	addRoute("GET", "/repos/{owner}/{repo}/commits", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		shaParam, hasSha := req.query["sha"]
		sha, found := resolveRef(r, req.query.Get("sha"))
		if !found {
			if hasSha && len(shaParam) > 0 {
				notFound()
			}
			fail(409, "Git Repository is empty.")
		}
		per, page := pageArgs(req.query)
		all := logFrom(r, sha, per*page+1)
		lo := (page - 1) * per
		if lo > len(all) {
			lo = len(all)
		}
		hi := lo + per
		if hi > len(all) {
			hi = len(all)
		}
		out := make([]J, 0, hi-lo)
		for _, oid := range all[lo:hi] {
			out = append(out, commitJSON(r, oid, req.base))
		}
		var h map[string]string
		if len(all) > page*per {
			q := url.Values{}
			for k, v := range req.query {
				q[k] = v
			}
			q.Set("page", strconv.Itoa(page+1))
			q.Set("per_page", strconv.Itoa(per))
			h = map[string]string{"Link": "<" + req.base + "/api/v3/repos/" + r.FullName() + "/commits?" + q.Encode() + `>; rel="next"`}
		}
		return restReply{status: 200, body: out, headers: h}
	})
	addRoute("GET", "/repos/{owner}/{repo}/commits/{ref*}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		sha, found := resolveRef(r, p[2])
		if !found {
			fail(422, "No commit found for SHA: "+p[2])
		}
		out := commitJSON(r, sha, req.base)
		if out == nil {
			notFound()
		}
		parent := ""
		if c := r.commit(sha); len(c.Parents) > 0 {
			parent = c.Parents[0]
		}
		out["files"] = diffCommits(r, parent, sha)
		return ok(out)
	})
	addRoute("GET", "/repos/{owner}/{repo}/git/commits/{sha}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		c := r.commit(p[2])
		if c == nil {
			notFound()
		}
		parents := []J{}
		for _, x := range c.Parents {
			parents = append(parents, J{"sha": x})
		}
		return ok(J{"sha": p[2], "tree": J{"sha": c.Tree}, "parents": parents, "message": strings.TrimSuffix(c.Message, "\n"),
			"author": parseIdent(c.Author), "committer": parseIdent(c.Committer), "url": req.base + "/api/v3/repos/" + r.FullName() + "/git/commits/" + p[2]})
	})
	addRoute("GET", "/repos/{owner}/{repo}/git/trees/{sha}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		sha := p[2]
		if c, found := resolveRef(r, sha); found {
			if cm := r.commit(c); cm != nil {
				sha = cm.Tree
			}
		}
		if o := r.Objects[sha]; o == nil || o.Type != "tree" {
			notFound()
		}
		recursive := req.query.Has("recursive")
		tree := []J{}
		var walk func(oid, prefix string)
		walk = func(oid, prefix string) {
			for _, e := range r.tree(oid) {
				dir := e.Mode == "40000"
				entry := J{"path": prefix + e.Name, "mode": e.Mode, "type": "blob", "sha": e.OID}
				if dir {
					entry["mode"] = "040000"
					entry["type"] = "tree"
				} else if o := r.Objects[e.OID]; o != nil {
					entry["size"] = len(o.Data)
				}
				tree = append(tree, entry)
				if dir && recursive {
					walk(e.OID, prefix+e.Name+"/")
				}
			}
		}
		walk(sha, "")
		return ok(J{"sha": sha, "tree": tree, "truncated": false})
	})
	addRoute("GET", "/repos/{owner}/{repo}/git/blobs/{sha}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		o := r.Objects[p[2]]
		if o == nil || o.Type != "blob" {
			notFound()
		}
		return ok(J{"sha": p[2], "size": len(o.Data), "encoding": "base64", "content": base64.StdEncoding.EncodeToString(o.Data)})
	})

	// ---- contents
	addRoute("GET", "/repos/{owner}/{repo}/contents", func(req *restReq, p []string) restReply { return getContents(req, p, "") })
	addRoute("GET", "/repos/{owner}/{repo}/contents/{path*}", func(req *restReq, p []string) restReply { return getContents(req, p, p[2]) })
	addRoute("PUT", "/repos/{owner}/{repo}/contents/{path*}", putContents)
	addRoute("DELETE", "/repos/{owner}/{repo}/contents/{path*}", deleteContents)

	// ---- labels
	addRoute("GET", "/repos/{owner}/{repo}/labels", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		return ok(labelsJSON(r.LabelOrder, r, req.base))
	})
	addRoute("POST", "/repos/{owner}/{repo}/labels", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		b := obj(req.body)
		name, _ := str(b, "name")
		if strings.TrimSpace(name) == "" {
			invalid("Validation Failed", J{"resource": "Label", "field": "name", "code": "missing_field"})
		}
		color, has := str(b, "color")
		if !has {
			color = "ededed"
		}
		var desc *string
		if d, has := str(b, "description"); has {
			desc = &d
		}
		l := addLabel(r, name, strings.TrimPrefix(color, "#"), desc)
		if l == nil {
			invalid("Validation Failed", J{"resource": "Label", "field": "name", "code": "already_exists"})
		}
		return restReply{status: 201, body: labelJSON(l, r, req.base)}
	})
	addRoute("GET", "/repos/{owner}/{repo}/labels/{name}", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		l := r.Labels[strings.ToLower(p[2])]
		if l == nil {
			notFound()
		}
		return ok(labelJSON(l, r, req.base))
	})

	// ---- issues
	addRoute("GET", "/repos/{owner}/{repo}/issues", listIssues)
	addRoute("PATCH", "/repos/{owner}/{repo}/issues/{number}", patchIssue)
	addRoute("GET", "/repos/{owner}/{repo}/issues/{number}/comments", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		is := issueOf(r, p[2])
		lo, hi, h := paginate(req, len(is.Comments), "/repos/"+r.FullName()+"/issues/"+p[2]+"/comments")
		out := make([]J, 0, hi-lo)
		for _, c := range is.Comments[lo:hi] {
			out = append(out, commentJSON(c, req.base))
		}
		return restReply{status: 200, body: out, headers: h}
	})
	addRoute("POST", "/repos/{owner}/{repo}/issues/{number}/comments", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		is := issueOf(r, p[2])
		body, _ := str(obj(req.body), "body")
		if body == "" {
			invalid("Validation Failed", J{"resource": "IssueComment", "field": "body", "code": "missing_field"})
		}
		c := addCommentTo(r, is.Number, &is.Comments, body)
		is.UpdatedAt = c.CreatedAt
		is.json = nil
		return restReply{status: 201, body: commentJSON(c, req.base)}
	})
	addRoute("GET", "/repos/{owner}/{repo}/issues/{number}/labels", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		return ok(labelsJSON(issueOf(r, p[2]).Labels, r, req.base))
	})
	addRoute("POST", "/repos/{owner}/{repo}/issues/{number}/labels", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		is := issueOf(r, p[2])
		raw, isList := req.body.([]any)
		if !isList {
			raw, _ = obj(req.body)["labels"].([]any)
		}
		labels, missing, found := resolveLabels(r, raw)
		if !found {
			invalid("Validation Failed: the label " + strconv.Quote(missing) + " does not exist")
		}
		for _, l := range labels {
			has := false
			for _, x := range is.Labels {
				if x == l {
					has = true
				}
			}
			if !has {
				is.Labels = append(is.Labels, l)
			}
		}
		is.json = nil
		return ok(labelsJSON(is.Labels, r, req.base))
	})

	// ---- pull requests
	addRoute("GET", "/repos/{owner}/{repo}/pulls", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		state := req.query.Get("state")
		if state == "" {
			state = "open"
		}
		head, base := req.query.Get("head"), req.query.Get("base")
		var items []*Pull
		for _, pl := range r.Pulls {
			if (state == "all" || pl.State == state) && (head == "" || pl.Head == branchName(head)) && (base == "" || pl.Base == base) {
				items = append(items, pl)
			}
		}
		sortPulls(items, req.query.Get("sort"), req.query.Get("direction"))
		lo, hi, h := paginate(req, len(items), "/repos/"+r.FullName()+"/pulls")
		out := make([]J, 0, hi-lo)
		for _, pl := range items[lo:hi] {
			out = append(out, pullJSON(pl, req.base, false))
		}
		return restReply{status: 200, body: out, headers: h}
	})
	addRoute("POST", "/repos/{owner}/{repo}/pulls", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		b := obj(req.body)
		title, _ := str(b, "title")
		head, hasHead := str(b, "head")
		base, hasBase := str(b, "base")
		if title == "" {
			invalid("Validation Failed", J{"resource": "PullRequest", "field": "title", "code": "missing_field"})
		}
		if !hasHead {
			invalid("Validation Failed", J{"resource": "PullRequest", "field": "head", "code": "missing_field"})
		}
		if !hasBase {
			invalid("Validation Failed", J{"resource": "PullRequest", "field": "base", "code": "missing_field"})
		}
		body, _ := str(b, "body")
		return restReply{status: 201, body: pullJSON(openPull(r, title, body, head, base), req.base, true)}
	})
	addRoute("GET", "/repos/{owner}/{repo}/pulls/{number}", func(req *restReq, p []string) restReply {
		return ok(pullJSON(pullOf(repoOf(p), p[2]), req.base, true))
	})
	addRoute("PATCH", "/repos/{owner}/{repo}/pulls/{number}", func(req *restReq, p []string) restReply {
		pl := pullOf(repoOf(p), p[2])
		b := obj(req.body)
		if v, has := str(b, "title"); has {
			pl.Title = v
		}
		if v, has := str(b, "body"); has {
			pl.Body = v
		}
		if v, _ := str(b, "state"); v == "open" || v == "closed" {
			setPullState(pl, v)
		}
		pl.UpdatedAt = nowISO()
		return ok(pullJSON(pl, req.base, true))
	})
	addRoute("GET", "/repos/{owner}/{repo}/pulls/{number}/files", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		pl := pullOf(r, p[2])
		files := pullFiles(pl)
		head := pullHeadSHA(pl)
		lo, hi, h := paginate(req, len(files), "/repos/"+r.FullName()+"/pulls/"+p[2]+"/files")
		out := make([]J, 0, hi-lo)
		for _, f := range files[lo:hi] {
			out = append(out, J{"sha": f.SHA, "filename": f.Filename, "status": f.Status, "additions": f.Additions, "deletions": f.Deletions,
				"changes": f.Changes, "blob_url": req.base + "/" + r.FullName() + "/blob/" + head + "/" + f.Filename,
				"raw_url":      req.base + "/" + r.FullName() + "/raw/" + head + "/" + f.Filename,
				"contents_url": req.base + "/api/v3/repos/" + r.FullName() + "/contents/" + f.Filename + "?ref=" + head})
		}
		return restReply{status: 200, body: out, headers: h}
	})
	addRoute("GET", "/repos/{owner}/{repo}/pulls/{number}/commits", func(req *restReq, p []string) restReply {
		r := repoOf(p)
		pl := pullOf(r, p[2])
		if head := pullHeadSHA(pl); head != "" {
			return ok([]J{commitJSON(r, head, req.base)})
		}
		return ok([]J{})
	})
	addRoute("GET", "/repos/{owner}/{repo}/pulls/{number}/merge", func(req *restReq, p []string) restReply {
		if !pullOf(repoOf(p), p[2]).Merged {
			notFound()
		}
		return restReply{status: 204}
	})
	addRoute("PUT", "/repos/{owner}/{repo}/pulls/{number}/merge", func(req *restReq, p []string) restReply {
		pl := pullOf(repoOf(p), p[2])
		b := obj(req.body)
		if sha, has := str(b, "sha"); has && sha != pullHeadSHA(pl) {
			fail(409, "Head branch was modified. Review and try the merge again.")
		}
		method, _ := str(b, "merge_method")
		if method == "" {
			method = "merge"
		}
		title, _ := str(b, "commit_title")
		message, _ := str(b, "commit_message")
		sha, status, msg := mergePull(pl, method, title, message)
		if status != 200 {
			fail(status, msg)
		}
		return ok(J{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
	})
}

func validRepoName(name string) bool {
	if name == "" || len(name) > 100 || strings.HasSuffix(name, ".git") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// resolveRef maps a ref name, branch, tag or sha to a commit sha ("" names the default branch).
func resolveRef(r *Repo, ref string) (string, bool) {
	if ref == "" {
		return r.branchTip(r.DefaultBranch)
	}
	for _, name := range []string{ref, "refs/heads/" + ref, "refs/tags/" + ref} {
		if oid, has := r.Refs[name]; has {
			return r.peel(oid), true
		}
	}
	if isOID(ref) && r.Objects[ref] != nil {
		return r.peel(ref), true
	}
	return "", false
}

// ---- contents handlers

func contentEntry(r *Repo, path, mode, sha, base, ref string) J {
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	dir := mode == "40000"
	api := base + "/api/v3/repos/" + r.FullName() + "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	typ, kind, size := "file", "blobs", 0
	var download any = base + "/raw/" + r.FullName() + "/" + ref + "/" + path
	if dir {
		typ, kind, download = "dir", "trees", nil
	} else if o := r.Objects[sha]; o != nil {
		size = len(o.Data)
	}
	return J{"type": typ, "name": name, "path": path, "sha": sha, "size": size, "url": api, "html_url": base + "/" + r.FullName() + "/blob/" + ref + "/" + path,
		"git_url": base + "/api/v3/repos/" + r.FullName() + "/git/" + kind + "/" + sha, "download_url": download, "_links": J{"self": api}}
}

func fileJSON(r *Repo, path, sha, base, ref string) J {
	out := contentEntry(r, path, "100644", sha, base, ref)
	out["encoding"] = "base64"
	content := ""
	if o := r.Objects[sha]; o != nil {
		content = base64.StdEncoding.EncodeToString(o.Data)
	}
	out["content"] = content
	return out
}

func getContents(req *restReq, p []string, raw string) restReply {
	r := repoOf(p)
	path := strings.Trim(raw, "/")
	refName := req.query.Get("ref")
	explicit := req.query.Has("ref")
	if refName == "" {
		refName = r.DefaultBranch
	}
	sha, found := resolveRef(r, refName)
	if !found {
		if !explicit {
			fail(404, "This repository is empty.")
		}
		fail(404, "No commit found for the ref "+refName)
	}
	c := r.commit(sha)
	if c == nil {
		notFound()
	}
	entry := lookupPath(r, c.Tree, path)
	if entry == nil {
		notFound()
	}
	if entry.Mode == "40000" {
		out := []J{}
		for _, child := range r.tree(entry.OID) {
			childPath := child.Name
			if path != "" {
				childPath = path + "/" + child.Name
			}
			out = append(out, contentEntry(r, childPath, child.Mode, child.OID, req.base, refName))
		}
		return ok(out)
	}
	return ok(fileJSON(r, path, entry.OID, req.base, refName))
}

func putContents(req *restReq, p []string) restReply {
	r := repoOf(p)
	path := strings.Trim(p[2], "/")
	b := obj(req.body)
	message, hasMsg := str(b, "message")
	content, hasContent := str(b, "content")
	if !hasMsg {
		invalid("Invalid request.\n\n\"message\" wasn't supplied.")
	}
	if !hasContent {
		invalid("Invalid request.\n\n\"content\" wasn't supplied.")
	}
	if path == "" {
		invalid("Invalid request.\n\npath is empty.")
	}
	branch, explicit := str(b, "branch")
	if !explicit || branch == "" {
		branch = r.DefaultBranch
	}
	tip, has := r.branchTip(branch)
	if !has && explicit && len(r.Refs) > 0 {
		from, ok := r.branchTip(r.DefaultBranch)
		if !ok {
			fail(404, "Branch "+branch+" not found")
		}
		r.setRef("refs/heads/"+branch, from)
		tip, has = from, true
	}
	var existing *TreeEntry
	if has {
		if c := r.commit(tip); c != nil {
			existing = lookupPath(r, c.Tree, path)
		}
	}
	if existing != nil && existing.Mode == "40000" {
		invalid("A directory exists at this path")
	}
	given, hasSha := str(b, "sha")
	if existing != nil {
		if !hasSha {
			invalid("Invalid request.\n\n\"sha\" wasn't supplied.")
		}
		if given != existing.OID {
			fail(409, path+" does not match "+given)
		}
	} else if hasSha && has {
		fail(409, path+" does not match "+given)
	}
	bytes, err := base64.StdEncoding.DecodeString(strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == ' ' {
			return -1
		}
		return c
	}, content))
	if err != nil {
		invalid("Invalid request.\n\ncontent is not valid Base64")
	}
	commit, blob := commitFile(r, branch, path, bytes, message)
	status := 201
	if existing != nil {
		status = 200
	}
	return restReply{status: status, body: J{"content": fileJSON(r, path, blob, req.base, branch), "commit": commitSummary(r, commit, req.base)}}
}

func deleteContents(req *restReq, p []string) restReply {
	r := repoOf(p)
	path := strings.Trim(p[2], "/")
	b := obj(req.body)
	message, has := str(b, "message")
	if !has {
		message = "Delete " + path
	}
	branch, explicit := str(b, "branch")
	if !explicit || branch == "" {
		branch = r.DefaultBranch
	}
	tip, hasTip := r.branchTip(branch)
	if !hasTip {
		notFound()
	}
	existing := lookupPath(r, r.commit(tip).Tree, path)
	if existing == nil || existing.Mode == "40000" {
		notFound()
	}
	given, hasSha := str(b, "sha")
	if !hasSha {
		invalid("Invalid request.\n\n\"sha\" wasn't supplied.")
	}
	if given != existing.OID {
		fail(409, path+" does not match "+given)
	}
	commit, _ := commitFile(r, branch, path, nil, message)
	return ok(J{"content": nil, "commit": commitSummary(r, commit, req.base)})
}

// ---- issue handlers

func issueOf(r *Repo, number string) *Issue {
	n, _ := strconv.Atoi(number)
	is := r.Issues[n]
	if is == nil {
		notFound()
	}
	return is
}

func sortIssueList(items []*Issue, sortBy, direction string) {
	sign := -1
	if direction == "asc" {
		sign = 1
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if sortBy == "updated" && a.UpdatedAt != b.UpdatedAt {
			return (a.UpdatedAt < b.UpdatedAt) == (sign == 1)
		}
		if sortBy == "comments" && len(a.Comments) != len(b.Comments) {
			return (len(a.Comments) < len(b.Comments)) == (sign == 1)
		}
		return (a.Number < b.Number) == (sign == 1)
	})
}

func sortPulls(items []*Pull, sortBy, direction string) {
	asc := direction == "asc"
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if sortBy == "updated" && a.UpdatedAt != b.UpdatedAt {
			return (a.UpdatedAt < b.UpdatedAt) == asc
		}
		return (a.Number < b.Number) == asc
	})
}

func listIssues(req *restReq, p []string) restReply {
	r := repoOf(p)
	state := req.query.Get("state")
	if state == "" {
		state = "open"
	}
	var wanted []string
	for _, l := range strings.Split(req.query.Get("labels"), ",") {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			wanted = append(wanted, l)
		}
	}
	var items []*Issue
	for _, is := range r.Issues {
		if state != "all" && is.State != state {
			continue
		}
		match := true
		for _, w := range wanted {
			found := false
			for _, l := range is.Labels {
				if strings.ToLower(l.Name) == w {
					found = true
				}
			}
			match = match && found
		}
		if match {
			items = append(items, is)
		}
	}
	sortBy := req.query.Get("sort")
	if sortBy == "" {
		sortBy = "created"
	}
	dir := req.query.Get("direction")
	if dir == "" {
		dir = "desc"
	}
	sortIssueList(items, sortBy, dir)
	lo, hi, h := paginate(req, len(items), "/repos/"+r.FullName()+"/issues")
	buf := []byte{'['}
	for i, is := range items[lo:hi] {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, issueJSON(is, req.base)...)
	}
	buf = append(buf, ']')
	return restReply{status: 200, raw: buf, headers: h}
}

func getIssue(req *restReq, p []string) restReply {
	r := repoOf(p)
	n, _ := strconv.Atoi(p[2])
	if is := r.Issues[n]; is != nil {
		return restReply{status: 200, raw: issueJSON(is, req.base)}
	}
	pl := r.Pulls[n]
	if pl == nil {
		notFound()
	}
	out := pullJSON(pl, req.base, false)
	out["pull_request"] = J{"url": req.base + "/api/v3/repos/" + r.FullName() + "/pulls/" + p[2]}
	out["comments"] = len(pl.Comments)
	return ok(out)
}

func postIssue(req *restReq, p []string) restReply {
	r := repoOf(p)
	b := obj(req.body)
	title, _ := str(b, "title")
	if n, isNum := b["title"].(float64); isNum {
		title = strconv.FormatFloat(n, 'f', -1, 64)
	}
	if title == "" {
		invalid("Validation Failed", J{"resource": "Issue", "field": "title", "code": "missing_field"})
	}
	var labels []*Label
	if list, isList := b["labels"].([]any); isList {
		ls, missing, found := resolveLabels(r, list)
		if !found {
			invalid("Validation Failed: the label "+strconv.Quote(missing)+" does not exist", J{"resource": "Label", "field": "name", "code": "invalid", "value": missing})
		}
		labels = ls
	}
	body, _ := str(b, "body")
	is := addIssue(r, title, body, labels)
	return restReply{status: 201, raw: issueJSON(is, req.base)}
}

func patchIssue(req *restReq, p []string) restReply {
	r := repoOf(p)
	is := issueOf(r, p[2])
	b := obj(req.body)
	if v, has := str(b, "title"); has {
		is.Title = v
	}
	if v, has := b["body"]; has {
		s, _ := v.(string)
		is.Body = s
	}
	if list, isList := b["labels"].([]any); isList {
		ls, missing, found := resolveLabels(r, list)
		if !found {
			invalid("Validation Failed: the label " + strconv.Quote(missing) + " does not exist")
		}
		is.Labels = ls
	}
	if v, has := b["state"]; has {
		switch v {
		case "open", "closed":
			setIssueState(is, v.(string))
		default:
			invalid("Validation Failed", J{"resource": "Issue", "field": "state", "code": "invalid"})
		}
	}
	is.UpdatedAt = nowISO()
	is.json = nil
	return restReply{status: 200, raw: issueJSON(is, req.base)}
}

// ---- pull helpers

func pullOf(r *Repo, number string) *Pull {
	n, _ := strconv.Atoi(number)
	pl := r.Pulls[n]
	if pl == nil {
		notFound()
	}
	return pl
}

func branchName(v string) string {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		return v[i+1:]
	}
	return v
}

func openPull(r *Repo, title, body, headRaw, baseRaw string) *Pull {
	head, base := branchName(headRaw), branchName(baseRaw)
	if _, has := r.branchTip(head); !has {
		invalid("Validation Failed", J{"resource": "PullRequest", "field": "head", "code": "invalid"})
	}
	if _, has := r.branchTip(base); !has {
		invalid("Validation Failed", J{"resource": "PullRequest", "field": "base", "code": "invalid"})
	}
	for _, pl := range r.Pulls {
		if pl.State == "open" && pl.Head == head && pl.Base == base {
			invalid("Validation Failed", J{"resource": "PullRequest", "code": "custom", "message": "A pull request already exists for " + r.Owner.Login + ":" + head + "."})
		}
	}
	return addPull(r, title, body, head, base)
}
