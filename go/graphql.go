// GitHub GraphQL v4 at /api/graphql: a hand-written parser and executor for the documents gh and
// github-mcp-server send. Unknown fields are reported in `errors` (HTTP 200); introspection answers
// __type and __schema; a JSON array body is operation batching.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ------------------------------------------------------------------ ordered output

type kv struct {
	k string
	v any
}
type ordered []kv

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := json.Marshal(e.k)
		b.Write(key)
		b.WriteByte(':')
		val, err := json.Marshal(e.v)
		if err != nil {
			return nil, err
		}
		b.Write(val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (o ordered) get(k string) (any, int) {
	for i, e := range o {
		if e.k == k {
			return e.v, i
		}
	}
	return nil, -1
}

// ------------------------------------------------------------------ lexer

type tok struct {
	kind string // punct, name, int, float, string, eof
	val  string
}

var numRe = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?`)

func lex(src string) ([]tok, error) {
	var out []tok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(src[i:], "..."):
			out = append(out, tok{"punct", "..."})
			i += 3
		case strings.IndexByte("!$&()+:=@[]{}|", c) >= 0:
			out = append(out, tok{"punct", string(c)})
			i++
		case c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			j := i
			for j < len(src) && (src[j] == '_' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			out = append(out, tok{"name", src[i:j]})
			i = j
		case c == '-' || c >= '0' && c <= '9':
			m := numRe.FindStringSubmatch(src[i:])
			if m == nil {
				return nil, fmt.Errorf("Unexpected character %q", c)
			}
			kind := "int"
			if m[2] != "" || m[3] != "" {
				kind = "float"
			}
			out = append(out, tok{kind, m[0]})
			i += len(m[0])
		case c == '"':
			if strings.HasPrefix(src[i:], `"""`) {
				end := strings.Index(src[i+3:], `"""`)
				if end < 0 {
					return nil, errors.New("Unterminated block string")
				}
				out = append(out, tok{"string", blockString(src[i+3 : i+3+end])})
				i += end + 6
				continue
			}
			var sb strings.Builder
			j := i + 1
			for {
				if j >= len(src) || src[j] == '\n' {
					return nil, errors.New("Unterminated string")
				}
				ch := src[j]
				if ch == '"' {
					break
				}
				if ch == '\\' && j+1 < len(src) {
					e := src[j+1]
					if e == 'u' && j+6 <= len(src) {
						n, _ := strconv.ParseUint(src[j+2:j+6], 16, 32)
						sb.WriteRune(rune(n))
						j += 6
						continue
					}
					switch e {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case 'b':
						sb.WriteByte('\b')
					case 'f':
						sb.WriteByte('\f')
					default:
						sb.WriteByte(e)
					}
					j += 2
					continue
				}
				sb.WriteByte(ch)
				j++
			}
			out = append(out, tok{"string", sb.String()})
			i = j + 1
		default:
			return nil, fmt.Errorf("Unexpected character %q", c)
		}
	}
	return append(out, tok{"eof", ""}), nil
}

func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, `\"""`, `"""`), "\n")
	indent := -1
	for _, l := range lines[1:] {
		trimmed := strings.TrimLeft(l, " \t")
		if trimmed == "" {
			continue
		}
		if n := len(l) - len(trimmed); indent < 0 || n < indent {
			indent = n
		}
	}
	for i := 1; i < len(lines) && indent > 0; i++ {
		if len(lines[i]) >= indent {
			lines[i] = lines[i][indent:]
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// ------------------------------------------------------------------ parser

type gValue struct {
	kind   string // var int float string enum bool null list object
	s      string
	b      bool
	list   []*gValue
	fields []gArg
}

type gArg struct {
	name string
	val  *gValue
}

type gDirective struct {
	name string
	args []gArg
}

type gSel struct {
	kind       string // field inline spread
	alias      string
	name       string
	args       []gArg
	directives []gDirective
	sels       []*gSel
	hasSels    bool
	on         string
}

type gVar struct {
	name, typ string
	def       *gValue
}

type gOp struct {
	typ, name string
	vars      []gVar
	sels      []*gSel
}

type gFragment struct {
	on   string
	sels []*gSel
}

type gDoc struct {
	ops       []*gOp
	fragments map[string]*gFragment
}

type parser struct {
	t []tok
	i int
}

type syntaxError struct{ msg string }

func (e syntaxError) Error() string { return e.msg }

func (p *parser) peek() tok { return p.t[p.i] }
func (p *parser) next() tok {
	t := p.t[p.i]
	if p.i < len(p.t)-1 {
		p.i++
	}
	return t
}
func (p *parser) is(v string) bool { t := p.peek(); return t.kind == "punct" && t.val == v }
func (p *parser) expect(v string) {
	t := p.next()
	if t.kind != "punct" || t.val != v {
		panic(syntaxError{fmt.Sprintf("Expected %s, found %s", v, orEOF(t.val))})
	}
}
func orEOF(s string) string {
	if s == "" {
		return "<EOF>"
	}
	return s
}
func (p *parser) name() string {
	t := p.next()
	if t.kind != "name" {
		panic(syntaxError{"Expected Name, found " + orEOF(t.val)})
	}
	return t.val
}

func (p *parser) document() *gDoc {
	d := &gDoc{fragments: map[string]*gFragment{}}
	for p.peek().kind != "eof" {
		if p.is("{") {
			d.ops = append(d.ops, &gOp{typ: "query", sels: p.selectionSet()})
			continue
		}
		word := p.name()
		if word == "fragment" {
			name := p.name()
			if p.name() != "on" {
				panic(syntaxError{`Expected "on"`})
			}
			on := p.name()
			p.directives()
			d.fragments[name] = &gFragment{on: on, sels: p.selectionSet()}
			continue
		}
		if word != "query" && word != "mutation" && word != "subscription" {
			panic(syntaxError{`Unexpected Name "` + word + `"`})
		}
		op := &gOp{typ: word}
		if p.peek().kind == "name" {
			op.name = p.name()
		}
		if p.is("(") {
			p.next()
			for !p.is(")") {
				if p.peek().kind == "eof" {
					panic(syntaxError{"Unexpected <EOF>"})
				}
				p.expect("$")
				v := gVar{name: p.name()}
				p.expect(":")
				v.typ = p.typeRef()
				if p.is("=") {
					p.next()
					v.def = p.value(true)
				}
				p.directives()
				op.vars = append(op.vars, v)
			}
			p.next()
		}
		p.directives()
		op.sels = p.selectionSet()
		d.ops = append(d.ops, op)
	}
	return d
}

func (p *parser) typeRef() string {
	var out string
	if p.is("[") {
		p.next()
		out = "[" + p.typeRef() + "]"
		p.expect("]")
	} else {
		out = p.name()
	}
	if p.is("!") {
		p.next()
		out += "!"
	}
	return out
}

func (p *parser) directives() []gDirective {
	var out []gDirective
	for p.is("@") {
		p.next()
		d := gDirective{name: p.name()}
		if p.is("(") {
			d.args = p.args()
		}
		out = append(out, d)
	}
	return out
}

func (p *parser) args() []gArg {
	p.expect("(")
	var out []gArg
	for !p.is(")") {
		if p.peek().kind == "eof" {
			panic(syntaxError{"Unexpected <EOF>"})
		}
		n := p.name()
		p.expect(":")
		out = append(out, gArg{n, p.value(false)})
	}
	p.next()
	return out
}

func (p *parser) value(constant bool) *gValue {
	t := p.peek()
	if t.kind == "punct" {
		switch {
		case t.val == "$" && !constant:
			p.next()
			return &gValue{kind: "var", s: p.name()}
		case t.val == "[":
			p.next()
			v := &gValue{kind: "list"}
			for !p.is("]") {
				if p.peek().kind == "eof" {
					panic(syntaxError{"Unexpected <EOF>"})
				}
				v.list = append(v.list, p.value(constant))
			}
			p.next()
			return v
		case t.val == "{":
			p.next()
			v := &gValue{kind: "object"}
			for !p.is("}") {
				if p.peek().kind == "eof" {
					panic(syntaxError{"Unexpected <EOF>"})
				}
				n := p.name()
				p.expect(":")
				v.fields = append(v.fields, gArg{n, p.value(constant)})
			}
			p.next()
			return v
		}
		panic(syntaxError{"Unexpected " + t.val})
	}
	p.next()
	switch t.kind {
	case "int", "float", "string":
		return &gValue{kind: t.kind, s: t.val}
	case "name":
		switch t.val {
		case "true", "false":
			return &gValue{kind: "bool", b: t.val == "true"}
		case "null":
			return &gValue{kind: "null"}
		}
		return &gValue{kind: "enum", s: t.val}
	}
	panic(syntaxError{"Unexpected <EOF>"})
}

func (p *parser) selectionSet() []*gSel {
	p.expect("{")
	var out []*gSel
	for !p.is("}") {
		if p.peek().kind == "eof" {
			panic(syntaxError{"Expected }, found <EOF>"})
		}
		if p.is("...") {
			p.next()
			if p.peek().kind == "name" && p.peek().val != "on" {
				name := p.name()
				out = append(out, &gSel{kind: "spread", name: name, directives: p.directives()})
				continue
			}
			s := &gSel{kind: "inline"}
			if p.peek().kind == "name" && p.peek().val == "on" {
				p.next()
				s.on = p.name()
			}
			s.directives = p.directives()
			s.sels = p.selectionSet()
			out = append(out, s)
			continue
		}
		s := &gSel{kind: "field", name: p.name()}
		if p.is(":") {
			p.next()
			s.alias = s.name
			s.name = p.name()
		}
		if p.is("(") {
			s.args = p.args()
		}
		s.directives = p.directives()
		if p.is("{") {
			s.sels = p.selectionSet()
			s.hasSels = true
		}
		out = append(out, s)
	}
	p.next()
	return out
}

func parseDocument(src string) (doc *gDoc, err error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	defer func() {
		if rec := recover(); rec != nil {
			if se, ok := rec.(syntaxError); ok {
				err = se
				return
			}
			panic(rec)
		}
	}()
	return (&parser{t: toks}).document(), nil
}

// ------------------------------------------------------------------ schema

type fieldDef struct {
	typ     string
	args    []string // "name:Type" pairs, for introspection
	resolve func(parent any, args J) (any, error)
}

type typeDef struct {
	kind          string
	fields        map[string]*fieldDef
	fieldOrder    []string
	enumValues    []string
	interfaces    []string
	possibleTypes []string
	inputFields   []string
}

type gqlFieldError struct {
	msg, typ string
}

func (e *gqlFieldError) Error() string { return e.msg }

func ferr(msg string) error { return &gqlFieldError{msg: msg, typ: "UNPROCESSABLE"} }

var types = map[string]*typeDef{}
var typeOrder []string

func defType(name, kind string) *typeDef {
	t := &typeDef{kind: kind, fields: map[string]*fieldDef{}}
	types[name] = t
	typeOrder = append(typeOrder, name)
	return t
}

func (t *typeDef) f(name, typ string, resolve func(parent any, args J) (any, error), args ...string) *typeDef {
	if _, exists := t.fields[name]; !exists {
		t.fieldOrder = append(t.fieldOrder, name)
	}
	t.fields[name] = &fieldDef{typ: typ, args: args, resolve: resolve}
	return t
}

// prop resolves a field from a J map parent.
func prop(key string) func(any, J) (any, error) {
	return func(parent any, _ J) (any, error) {
		if m, ok := parent.(J); ok {
			return m[key], nil
		}
		return nil, nil
	}
}

func cursorOf(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:v2:" + strconv.Itoa(i)))
}

func decodeCursor(v any) (int, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || !strings.HasPrefix(string(raw), "cursor:v2:") {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw)[10:])
	return n, err == nil
}

func num(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	}
	return 0, false
}

func connection[T any](items []T, args J) (any, error) {
	start, end := 0, len(items)
	if a, ok := decodeCursor(args["after"]); ok && a+1 > start {
		start = a + 1
	}
	if b, ok := decodeCursor(args["before"]); ok && b < end {
		end = b
	}
	if start > len(items) {
		start = len(items)
	}
	if end < start {
		end = start
	}
	if f, ok := num(args["first"]); ok {
		if f < 0 {
			return nil, &gqlFieldError{"`first` must be non-negative", "INVALID_ARGUMENT"}
		}
		if start+f < end {
			end = start + f
		}
	}
	if l, ok := num(args["last"]); ok && end-l > start {
		start = end - l
	}
	slice := items[start:end]
	nodes := make([]any, len(slice))
	edges := make([]any, len(slice))
	for i, n := range slice {
		nodes[i] = n
		edges[i] = J{"cursor": cursorOf(start + i), "node": n}
	}
	var sc, ec any
	if len(slice) > 0 {
		sc, ec = cursorOf(start), cursorOf(end-1)
	}
	return J{"nodes": nodes, "edges": edges, "totalCount": len(items),
		"pageInfo": J{"hasNextPage": end < len(items), "hasPreviousPage": start > 0, "startCursor": sc, "endCursor": ec}}, nil
}

var connArgs = []string{"first:Int", "after:String", "last:Int", "before:String"}

func connectionType(node string) {
	defType(node+"Connection", "OBJECT").
		f("nodes", "["+node+"]", prop("nodes")).
		f("edges", "["+node+"Edge]", prop("edges")).
		f("totalCount", "Int!", prop("totalCount")).
		f("pageInfo", "PageInfo!", prop("pageInfo"))
	defType(node+"Edge", "OBJECT").f("cursor", "String!", prop("cursor")).f("node", node, prop("node"))
}

func issueState(is *Issue) string {
	if is.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func commitNode(r *Repo, oid string) any {
	if oid == "" {
		return nil
	}
	c := r.commit(oid)
	if c == nil {
		return nil
	}
	a, cm := parseIdent(c.Author), parseIdent(c.Committer)
	msg := strings.TrimSuffix(c.Message, "\n")
	lines := strings.Split(msg, "\n")
	body := ""
	if len(lines) > 2 {
		body = strings.Join(lines[2:], "\n")
	}
	return J{"__typename": "Commit", "id": "C_" + oid, "oid": oid, "abbreviatedOid": oid[:7], "message": msg, "messageHeadline": lines[0], "messageBody": body,
		"committedDate": cm.Date, "authoredDate": a.Date, "url": "/" + r.FullName() + "/commit/" + oid, "tree": J{"oid": c.Tree},
		"author": J{"name": a.Name, "email": a.Email, "date": a.Date}, "committer": J{"name": cm.Name, "email": cm.Email, "date": cm.Date}}
}

func refNode(r *Repo, name string) any {
	oid, has := r.Refs[name]
	if !has {
		return nil
	}
	prefix := ""
	switch {
	case strings.HasPrefix(name, "refs/heads/"):
		prefix = "refs/heads/"
	case strings.HasPrefix(name, "refs/tags/"):
		prefix = "refs/tags/"
	default:
		prefix = name[:strings.LastIndexByte(name, '/')+1]
	}
	return J{"__typename": "Ref", "id": "REF_" + base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(r.ID)+":"+name)), "name": name[len(prefix):],
		"prefix": prefix, "target": commitNode(r, r.peel(oid)), "repository": r}
}

func orderBy(args J) (string, bool) {
	ob, ok := args["orderBy"].(map[string]any)
	if !ok {
		return "created", true
	}
	field, _ := ob["field"].(string)
	dir, _ := ob["direction"].(string)
	sortBy := "created"
	if field == "UPDATED_AT" {
		sortBy = "updated"
	} else if field == "COMMENTS" {
		sortBy = "comments"
	}
	return sortBy, dir != "DESC"
}

func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func labelsMatch(labels []*Label, wanted []string) bool {
	for _, w := range wanted {
		found := false
		for _, l := range labels {
			if strings.EqualFold(l.Name, w) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func sortedIssues(r *Repo) []*Issue {
	out := make([]*Issue, 0, len(r.Issues))
	for _, is := range r.Issues {
		out = append(out, is)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func init() {
	q := defType("Query", "OBJECT")
	defType("Mutation", "OBJECT")
	q.f("viewer", "User!", func(any, J) (any, error) { return viewer, nil })
	userByLogin := func(_ any, a J) (any, error) {
		if s, _ := a["login"].(string); strings.EqualFold(s, viewer.Login) {
			return viewer, nil
		}
		return nil, nil
	}
	q.f("user", "User", userByLogin, "login:String!")
	q.f("repositoryOwner", "User", userByLogin, "login:String!")
	q.f("repository", "Repository", func(_ any, a J) (any, error) {
		owner, _ := a["owner"].(string)
		name, _ := a["name"].(string)
		if r := findRepo(owner, name); r != nil {
			return r, nil
		}
		return nil, nil
	}, "owner:String!", "name:String!", "followRenames:Boolean")
	q.f("node", "Node", func(_ any, a J) (any, error) { return fromGlobalID(a["id"]), nil }, "id:ID!")
	q.f("nodes", "[Node]!", func(_ any, a J) (any, error) {
		list, _ := a["ids"].([]any)
		out := make([]any, len(list))
		for i, id := range list {
			out[i] = fromGlobalID(id)
		}
		return out, nil
	}, "ids:[ID!]!")
	q.f("rateLimit", "RateLimit", func(any, J) (any, error) {
		return J{"limit": 5000, "cost": 1, "remaining": 4999, "resetAt": "2030-01-01T00:00:00Z", "used": 1}, nil
	})

	node := defType("Node", "INTERFACE")
	node.f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil })
	node.possibleTypes = []string{"Repository", "Issue", "PullRequest", "IssueComment", "Label", "User"}
	defType("IssueOrPullRequest", "UNION").possibleTypes = []string{"Issue", "PullRequest"}

	u := defType("User", "OBJECT")
	u.interfaces = []string{"Node", "Actor", "RepositoryOwner"}
	u.f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil }).
		f("databaseId", "Int", func(p any, _ J) (any, error) { return p.(*User).ID, nil }).
		f("login", "String!", func(p any, _ J) (any, error) { return p.(*User).Login, nil }).
		f("name", "String", func(p any, _ J) (any, error) { return p.(*User).Name, nil }).
		f("email", "String!", func(p any, _ J) (any, error) { return p.(*User).Email, nil }).
		f("url", "URI!", func(p any, _ J) (any, error) { return "/" + p.(*User).Login, nil }).
		f("avatarUrl", "URI!", func(p any, _ J) (any, error) { return "/avatars/" + p.(*User).Login, nil }, "size:Int").
		f("repositories", "RepositoryConnection!", func(_ any, a J) (any, error) { return connection(listRepos(), a) }, append(connArgs, "privacy:RepositoryPrivacy")...).
		f("repository", "Repository", func(p any, a J) (any, error) {
			name, _ := a["name"].(string)
			if r := findRepo(p.(*User).Login, name); r != nil {
				return r, nil
			}
			return nil, nil
		}, "name:String!")

	rp := defType("Repository", "OBJECT")
	rp.interfaces = []string{"Node"}
	R := func(fn func(r *Repo, a J) any) func(any, J) (any, error) {
		return func(p any, a J) (any, error) { return fn(p.(*Repo), a), nil }
	}
	rp.f("id", "ID!", R(func(r *Repo, _ J) any { return globalIDOf(r) })).
		f("databaseId", "Int", R(func(r *Repo, _ J) any { return r.ID })).
		f("name", "String!", R(func(r *Repo, _ J) any { return r.Name })).
		f("nameWithOwner", "String!", R(func(r *Repo, _ J) any { return r.FullName() })).
		f("owner", "User!", R(func(r *Repo, _ J) any { return r.Owner })).
		f("description", "String", R(func(r *Repo, _ J) any { return r.Description })).
		f("url", "URI!", R(func(r *Repo, _ J) any { return "/" + r.FullName() })).
		f("sshUrl", "String!", R(func(r *Repo, _ J) any { return "/" + r.FullName() + ".git" })).
		f("isPrivate", "Boolean!", R(func(r *Repo, _ J) any { return r.Private })).
		f("visibility", "RepositoryVisibility!", R(func(r *Repo, _ J) any {
			if r.Private {
				return "PRIVATE"
			}
			return "PUBLIC"
		})).
		f("isFork", "Boolean!", R(func(*Repo, J) any { return false })).
		f("isArchived", "Boolean!", R(func(*Repo, J) any { return false })).
		f("isEmpty", "Boolean!", R(func(r *Repo, _ J) any { return len(r.Refs) == 0 })).
		f("isTemplate", "Boolean!", R(func(*Repo, J) any { return false })).
		f("createdAt", "DateTime!", R(func(r *Repo, _ J) any { return r.CreatedAt })).
		f("updatedAt", "DateTime!", R(func(r *Repo, _ J) any { return r.UpdatedAt })).
		f("pushedAt", "DateTime", R(func(r *Repo, _ J) any { return r.PushedAt })).
		f("stargazerCount", "Int!", R(func(*Repo, J) any { return 0 })).
		f("forkCount", "Int!", R(func(*Repo, J) any { return 0 })).
		f("viewerPermission", "RepositoryPermission", R(func(*Repo, J) any { return "ADMIN" })).
		f("viewerCanAdminister", "Boolean!", R(func(*Repo, J) any { return true })).
		f("hasIssuesEnabled", "Boolean!", R(func(*Repo, J) any { return true })).
		f("hasWikiEnabled", "Boolean!", R(func(*Repo, J) any { return false })).
		f("hasProjectsEnabled", "Boolean!", R(func(*Repo, J) any { return false })).
		f("mergeCommitAllowed", "Boolean!", R(func(*Repo, J) any { return true })).
		f("squashMergeAllowed", "Boolean!", R(func(*Repo, J) any { return true })).
		f("rebaseMergeAllowed", "Boolean!", R(func(*Repo, J) any { return true })).
		f("defaultBranchRef", "Ref", R(func(r *Repo, _ J) any { return refNode(r, "refs/heads/"+r.DefaultBranch) })).
		f("ref", "Ref", R(func(r *Repo, a J) any {
			name, _ := a["qualifiedName"].(string)
			if !strings.HasPrefix(name, "refs/") {
				name = "refs/heads/" + name
			}
			return refNode(r, name)
		}), "qualifiedName:String!").
		f("refs", "RefConnection", func(p any, a J) (any, error) {
			r := p.(*Repo)
			prefix, _ := a["refPrefix"].(string)
			if prefix == "" {
				prefix = "refs/"
			}
			var list []any
			for _, n := range r.sortedRefs() {
				if strings.HasPrefix(n, prefix) {
					list = append(list, refNode(r, n))
				}
			}
			return connection(list, a)
		}, append([]string{"refPrefix:String!"}, connArgs...)...).
		f("issue", "Issue", func(p any, a J) (any, error) {
			n, _ := num(a["number"])
			if is := p.(*Repo).Issues[n]; is != nil {
				return is, nil
			}
			return nil, nil
		}, "number:Int!").
		f("issues", "IssueConnection!", func(p any, a J) (any, error) {
			r := p.(*Repo)
			states := stringList(a["states"])
			var filterStates []string
			if fb, ok := a["filterBy"].(map[string]any); ok {
				filterStates = stringList(fb["states"])
			}
			wanted := stringList(a["labels"])
			var items []*Issue
			for _, is := range r.Issues {
				st := issueState(is)
				if (len(states) == 0 || contains(states, st)) && (len(filterStates) == 0 || contains(filterStates, st)) && labelsMatch(is.Labels, wanted) {
					items = append(items, is)
				}
			}
			sortBy, asc := orderBy(a)
			dir := "desc"
			if asc {
				dir = "asc"
			}
			sortIssueList(items, sortBy, dir)
			return connection(items, a)
		}, append(connArgs, "states:[IssueState!]", "orderBy:IssueOrder", "labels:[String!]", "filterBy:IssueFilters")...).
		f("pullRequest", "PullRequest", func(p any, a J) (any, error) {
			n, _ := num(a["number"])
			if pl := p.(*Repo).Pulls[n]; pl != nil {
				return pl, nil
			}
			return nil, nil
		}, "number:Int!").
		f("pullRequests", "PullRequestConnection!", func(p any, a J) (any, error) {
			r := p.(*Repo)
			states := stringList(a["states"])
			head, hasHead := a["headRefName"].(string)
			base, hasBase := a["baseRefName"].(string)
			var items []*Pull
			for _, pl := range r.Pulls {
				if (len(states) == 0 || contains(states, pullState(pl))) && (!hasHead || pl.Head == head) && (!hasBase || pl.Base == base) {
					items = append(items, pl)
				}
			}
			sortBy, asc := orderBy(a)
			dir := "desc"
			if asc {
				dir = "asc"
			}
			sortPulls(items, sortBy, dir)
			return connection(items, a)
		}, append(connArgs, "states:[PullRequestState!]", "orderBy:IssueOrder", "headRefName:String", "baseRefName:String", "labels:[String!]")...).
		f("issueOrPullRequest", "IssueOrPullRequest", func(p any, a J) (any, error) {
			n, _ := num(a["number"])
			r := p.(*Repo)
			if is := r.Issues[n]; is != nil {
				return is, nil
			}
			if pl := r.Pulls[n]; pl != nil {
				return pl, nil
			}
			return nil, nil
		}, "number:Int!").
		f("labels", "LabelConnection", func(p any, a J) (any, error) { return connection(p.(*Repo).LabelOrder, a) }, append(connArgs, "query:String")...).
		f("label", "Label", func(p any, a J) (any, error) {
			name, _ := a["name"].(string)
			if l := p.(*Repo).Labels[strings.ToLower(name)]; l != nil {
				return l, nil
			}
			return nil, nil
		}, "name:String!").
		f("assignableUsers", "UserConnection!", func(_ any, a J) (any, error) { return connection([]*User{viewer}, a) }, connArgs...)

	defType("Ref", "OBJECT").f("id", "ID!", prop("id")).f("name", "String!", prop("name")).f("prefix", "String!", prop("prefix")).
		f("target", "Commit", prop("target")).f("repository", "Repository!", prop("repository")).interfaces = []string{"Node"}
	cm := defType("Commit", "OBJECT")
	cm.interfaces = []string{"Node"}
	for _, f := range []string{"id:ID!", "oid:GitObjectID!", "abbreviatedOid:String!", "message:String!", "messageHeadline:String!", "messageBody:String!",
		"committedDate:DateTime!", "authoredDate:DateTime!", "url:URI!", "tree:Tree!", "author:GitActor", "committer:GitActor"} {
		parts := strings.SplitN(f, ":", 2)
		cm.f(parts[0], parts[1], prop(parts[0]))
	}
	defType("Tree", "OBJECT").f("oid", "GitObjectID!", prop("oid"))
	defType("GitActor", "OBJECT").f("name", "String", prop("name")).f("email", "String", prop("email")).f("date", "GitTimestamp", prop("date"))

	// Fields shared by Issue and PullRequest.
	shared := func(t *typeDef, isIssue bool) {
		repoOfNode := func(p any) *Repo {
			if is, ok := p.(*Issue); ok {
				return is.Repo
			}
			return p.(*Pull).Repo
		}
		get := func(fn func(is *Issue, pl *Pull) any) func(any, J) (any, error) {
			return func(p any, _ J) (any, error) {
				is, _ := p.(*Issue)
				pl, _ := p.(*Pull)
				return fn(is, pl), nil
			}
		}
		t.f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil }).
			f("databaseId", "Int", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.ID
				}
				return pl.ID
			})).
			f("number", "Int!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.Number
				}
				return pl.Number
			})).
			f("title", "String!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.Title
				}
				return pl.Title
			})).
			f("body", "String!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.Body
				}
				return pl.Body
			})).
			f("bodyText", "String!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.Body
				}
				return pl.Body
			})).
			f("createdAt", "DateTime!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.CreatedAt
				}
				return pl.CreatedAt
			})).
			f("updatedAt", "DateTime!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.UpdatedAt
				}
				return pl.UpdatedAt
			})).
			f("closedAt", "DateTime", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.ClosedAt
				}
				return pl.ClosedAt
			})).
			f("closed", "Boolean!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return is.State == "closed"
				}
				return pl.State == "closed"
			})).
			f("locked", "Boolean!", get(func(*Issue, *Pull) any { return false })).
			f("viewerDidAuthor", "Boolean!", get(func(*Issue, *Pull) any { return true })).
			f("author", "User", get(func(*Issue, *Pull) any { return viewer })).
			f("repository", "Repository!", func(p any, _ J) (any, error) { return repoOfNode(p), nil }).
			f("url", "URI!", get(func(is *Issue, pl *Pull) any {
				if is != nil {
					return "/" + is.Repo.FullName() + "/issues/" + strconv.Itoa(is.Number)
				}
				return "/" + pl.Repo.FullName() + "/pull/" + strconv.Itoa(pl.Number)
			})).
			f("comments", "IssueCommentConnection!", func(p any, a J) (any, error) {
				if is, ok := p.(*Issue); ok {
					return connection(is.Comments, a)
				}
				return connection(p.(*Pull).Comments, a)
			}, connArgs...).
			f("labels", "LabelConnection", func(p any, a J) (any, error) {
				if is, ok := p.(*Issue); ok {
					return connection(is.Labels, a)
				}
				return connection([]*Label{}, a)
			}, connArgs...).
			f("assignees", "UserConnection!", func(_ any, a J) (any, error) { return connection([]*User{}, a) }, connArgs...)
		_ = isIssue
	}
	is := defType("Issue", "OBJECT")
	is.interfaces = []string{"Node"}
	shared(is, true)
	is.f("state", "IssueState!", func(p any, _ J) (any, error) { return issueState(p.(*Issue)), nil }).
		f("stateReason", "IssueStateReason", func(p any, _ J) (any, error) {
			if r := p.(*Issue).StateReason; r != nil {
				return strings.ToUpper(*r), nil
			}
			return nil, nil
		})
	pr := defType("PullRequest", "OBJECT")
	pr.interfaces = []string{"Node"}
	shared(pr, false)
	P := func(fn func(p *Pull, a J) any) func(any, J) (any, error) {
		return func(p any, a J) (any, error) { return fn(p.(*Pull), a), nil }
	}
	pr.f("state", "PullRequestState!", P(func(p *Pull, _ J) any { return pullState(p) })).
		f("merged", "Boolean!", P(func(p *Pull, _ J) any { return p.Merged })).
		f("mergedAt", "DateTime", P(func(p *Pull, _ J) any { return p.MergedAt })).
		f("isDraft", "Boolean!", P(func(*Pull, J) any { return false })).
		f("headRefName", "String!", P(func(p *Pull, _ J) any { return p.Head })).
		f("baseRefName", "String!", P(func(p *Pull, _ J) any { return p.Base })).
		f("headRefOid", "GitObjectID!", P(func(p *Pull, _ J) any { return pullHeadSHA(p) })).
		f("baseRefOid", "GitObjectID!", P(func(p *Pull, _ J) any { return pullBaseSHA(p) })).
		f("headRepository", "Repository", P(func(p *Pull, _ J) any { return p.Repo })).
		f("baseRepository", "Repository", P(func(p *Pull, _ J) any { return p.Repo })).
		f("headRef", "Ref", P(func(p *Pull, _ J) any { return refNode(p.Repo, "refs/heads/"+p.Head) })).
		f("baseRef", "Ref", P(func(p *Pull, _ J) any { return refNode(p.Repo, "refs/heads/"+p.Base) })).
		f("mergeable", "MergeableState!", P(func(p *Pull, _ J) any {
			if p.Merged {
				return "UNKNOWN"
			}
			if m := pullMergeable(p); m != nil && *m {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		})).
		f("mergeCommit", "Commit", P(func(p *Pull, _ J) any {
			if p.MergeCommitSHA == nil {
				return nil
			}
			return commitNode(p.Repo, *p.MergeCommitSHA)
		})).
		f("additions", "Int!", P(func(p *Pull, _ J) any {
			n := 0
			for _, f := range pullFiles(p) {
				n += f.Additions
			}
			return n
		})).
		f("deletions", "Int!", P(func(p *Pull, _ J) any {
			n := 0
			for _, f := range pullFiles(p) {
				n += f.Deletions
			}
			return n
		})).
		f("changedFiles", "Int!", P(func(p *Pull, _ J) any { return len(pullFiles(p)) })).
		f("files", "PullRequestChangedFileConnection", func(p any, a J) (any, error) {
			var list []J
			for _, f := range pullFiles(p.(*Pull)) {
				change := "MODIFIED"
				if f.Status == "added" {
					change = "ADDED"
				} else if f.Status == "removed" {
					change = "DELETED"
				}
				list = append(list, J{"path": f.Filename, "additions": f.Additions, "deletions": f.Deletions, "changeType": change})
			}
			return connection(list, a)
		}, connArgs...).
		f("commits", "PullRequestCommitConnection!", func(p any, a J) (any, error) {
			pl := p.(*Pull)
			return connection([]J{{"commit": commitNode(pl.Repo, pullHeadSHA(pl))}}, a)
		}, connArgs...)
	defType("PullRequestChangedFile", "OBJECT").f("path", "String!", prop("path")).f("additions", "Int!", prop("additions")).
		f("deletions", "Int!", prop("deletions")).f("changeType", "PatchStatus!", prop("changeType"))
	defType("PullRequestCommit", "OBJECT").f("commit", "Commit!", prop("commit"))
	ic := defType("IssueComment", "OBJECT")
	ic.interfaces = []string{"Node"}
	ic.f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil }).
		f("databaseId", "Int", func(p any, _ J) (any, error) { return p.(*Comment).ID, nil }).
		f("body", "String!", func(p any, _ J) (any, error) { return p.(*Comment).Body, nil }).
		f("createdAt", "DateTime!", func(p any, _ J) (any, error) { return p.(*Comment).CreatedAt, nil }).
		f("updatedAt", "DateTime!", func(p any, _ J) (any, error) { return p.(*Comment).UpdatedAt, nil }).
		f("author", "User", func(any, J) (any, error) { return viewer, nil }).
		f("url", "URI!", func(p any, _ J) (any, error) {
			c := p.(*Comment)
			return "/" + c.Repo.FullName() + "/issues/" + strconv.Itoa(c.Number) + "#issuecomment-" + strconv.Itoa(c.ID), nil
		})
	lb := defType("Label", "OBJECT")
	lb.interfaces = []string{"Node"}
	lb.f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil }).
		f("name", "String!", func(p any, _ J) (any, error) { return p.(*Label).Name, nil }).
		f("color", "String!", func(p any, _ J) (any, error) { return p.(*Label).Color, nil }).
		f("description", "String", func(p any, _ J) (any, error) { return p.(*Label).Description, nil })
	defType("PageInfo", "OBJECT").f("hasNextPage", "Boolean!", prop("hasNextPage")).f("hasPreviousPage", "Boolean!", prop("hasPreviousPage")).
		f("startCursor", "String", prop("startCursor")).f("endCursor", "String", prop("endCursor"))
	defType("RateLimit", "OBJECT").f("limit", "Int!", prop("limit")).f("cost", "Int!", prop("cost")).f("remaining", "Int!", prop("remaining")).
		f("resetAt", "DateTime!", prop("resetAt")).f("used", "Int!", prop("used"))
	for _, n := range []string{"Issue", "PullRequest", "IssueComment", "Label", "User", "Repository", "Ref", "PullRequestChangedFile", "PullRequestCommit"} {
		connectionType(n)
	}
	enums := map[string][]string{
		"IssueState": {"OPEN", "CLOSED"}, "IssueStateReason": {"COMPLETED", "NOT_PLANNED", "REOPENED", "DUPLICATE"},
		"PullRequestState": {"OPEN", "CLOSED", "MERGED"}, "MergeableState": {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
		"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"}, "RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
		"RepositoryVisibility": {"PRIVATE", "PUBLIC", "INTERNAL"}, "RepositoryPrivacy": {"PRIVATE", "PUBLIC"},
		"SearchType": {"ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"}, "IssueOrderField": {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
		"OrderDirection": {"ASC", "DESC"}, "PatchStatus": {"ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED"},
	}
	enumNames := make([]string, 0, len(enums))
	for n := range enums {
		enumNames = append(enumNames, n)
	}
	sort.Strings(enumNames)
	for _, n := range enumNames {
		defType(n, "ENUM").enumValues = enums[n]
	}
	defType("IssueOrder", "INPUT_OBJECT").inputFields = []string{"field:IssueOrderField!", "direction:OrderDirection!"}
	defType("IssueFilters", "INPUT_OBJECT").inputFields = []string{"states:[IssueState!]", "labels:[String!]"}
	actor := defType("Actor", "INTERFACE")
	actor.f("login", "String!", func(p any, _ J) (any, error) { return p.(*User).Login, nil })
	actor.possibleTypes = []string{"User"}
	owner := defType("RepositoryOwner", "INTERFACE")
	owner.f("login", "String!", func(p any, _ J) (any, error) { return p.(*User).Login, nil }).f("id", "ID!", func(p any, _ J) (any, error) { return globalIDOf(p), nil })
	owner.possibleTypes = []string{"User"}
	for _, s := range []string{"ID", "String", "Int", "Float", "Boolean", "DateTime", "GitTimestamp", "GitObjectID", "URI"} {
		defType(s, "SCALAR")
	}
	defineMutations()
}

// ------------------------------------------------------------------ mutations

func nodeOfKind[T any](id any) (T, error) {
	var zero T
	found := fromGlobalID(id)
	if v, ok := found.(T); ok && found != nil {
		return v, nil
	}
	return zero, &gqlFieldError{fmt.Sprintf("Could not resolve to a node with the global id of '%v'", id), "NOT_FOUND"}
}

func inputOf(a J) (J, error) {
	in, ok := a["input"].(map[string]any)
	if !ok {
		return nil, &gqlFieldError{"Argument input is required", "INVALID_ARGUMENT"}
	}
	return in, nil
}

// wrapRest runs a REST helper that reports failure by panicking with *httpError.
func wrapRest(run func()) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if he, ok := rec.(*httpError); ok {
				msg := he.msg
				if list, ok := he.extra["errors"].([]any); ok {
					var parts []string
					for _, e := range list {
						if m, ok := e.(J); ok {
							if s, ok := m["message"].(string); ok {
								parts = append(parts, s)
							}
						}
					}
					if len(parts) > 0 {
						msg = strings.Join(parts, "; ")
					}
				}
				err = ferr(msg)
				return
			}
			panic(rec)
		}
	}()
	run()
	return nil
}

func payloadType(name string, fields ...string) {
	t := defType(name, "OBJECT")
	for _, f := range fields {
		parts := strings.SplitN(f, ":", 2)
		t.f(parts[0], parts[1], prop(parts[0]))
	}
}

func defineMutations() {
	payloadType("CreateIssuePayload", "issue:Issue", "clientMutationId:String")
	payloadType("CloseIssuePayload", "issue:Issue", "clientMutationId:String")
	payloadType("ReopenIssuePayload", "issue:Issue", "clientMutationId:String")
	payloadType("UpdateIssuePayload", "issue:Issue", "clientMutationId:String")
	payloadType("CreatePullRequestPayload", "pullRequest:PullRequest", "clientMutationId:String")
	payloadType("MergePullRequestPayload", "pullRequest:PullRequest", "clientMutationId:String")
	payloadType("ClosePullRequestPayload", "pullRequest:PullRequest", "clientMutationId:String")
	payloadType("ReopenPullRequestPayload", "pullRequest:PullRequest", "clientMutationId:String")
	payloadType("AddCommentPayload", "commentEdge:IssueCommentEdge", "subject:Node", "timelineEdge:IssueCommentEdge", "clientMutationId:String")
	for _, n := range []string{"CreateIssueInput", "CloseIssueInput", "ReopenIssueInput", "UpdateIssueInput", "CreatePullRequestInput", "MergePullRequestInput",
		"ClosePullRequestInput", "ReopenPullRequestInput", "AddCommentInput"} {
		defType(n, "INPUT_OBJECT").inputFields = []string{"clientMutationId:String"}
	}
	m := types["Mutation"]
	m.f("createIssue", "CreateIssuePayload", func(_ any, a J) (any, error) {
		in, err := inputOf(a)
		if err != nil {
			return nil, err
		}
		r, err := nodeOfKind[*Repo](in["repositoryId"])
		if err != nil {
			return nil, err
		}
		title, _ := in["title"].(string)
		if title == "" {
			return nil, ferr("Title can't be blank")
		}
		var labels []*Label
		for _, id := range anyList(in["labelIds"]) {
			l, err := nodeOfKind[*Label](id)
			if err != nil {
				return nil, err
			}
			labels = append(labels, l)
		}
		body, _ := in["body"].(string)
		return J{"issue": addIssue(r, title, body, labels), "clientMutationId": in["clientMutationId"]}, nil
	}, "input:CreateIssueInput!")
	issueState := func(state string) func(any, J) (any, error) {
		return func(_ any, a J) (any, error) {
			in, err := inputOf(a)
			if err != nil {
				return nil, err
			}
			is, err := nodeOfKind[*Issue](in["issueId"])
			if err != nil {
				return nil, err
			}
			setIssueState(is, state)
			if in["stateReason"] == "NOT_PLANNED" {
				reason := "not_planned"
				is.StateReason = &reason
			}
			return J{"issue": is, "clientMutationId": in["clientMutationId"]}, nil
		}
	}
	m.f("closeIssue", "CloseIssuePayload", issueState("closed"), "input:CloseIssueInput!")
	m.f("reopenIssue", "ReopenIssuePayload", issueState("open"), "input:ReopenIssueInput!")
	m.f("updateIssue", "UpdateIssuePayload", func(_ any, a J) (any, error) {
		in, err := inputOf(a)
		if err != nil {
			return nil, err
		}
		is, err := nodeOfKind[*Issue](in["id"])
		if err != nil {
			return nil, err
		}
		if v, ok := in["title"].(string); ok {
			is.Title = v
		}
		if v, ok := in["body"].(string); ok {
			is.Body = v
		}
		switch in["state"] {
		case "OPEN":
			setIssueState(is, "open")
		case "CLOSED":
			setIssueState(is, "closed")
		}
		is.json = nil
		return J{"issue": is, "clientMutationId": in["clientMutationId"]}, nil
	}, "input:UpdateIssueInput!")
	m.f("createPullRequest", "CreatePullRequestPayload", func(_ any, a J) (any, error) {
		in, err := inputOf(a)
		if err != nil {
			return nil, err
		}
		r, err := nodeOfKind[*Repo](in["repositoryId"])
		if err != nil {
			return nil, err
		}
		title, _ := in["title"].(string)
		if title == "" {
			return nil, ferr("Title can't be blank")
		}
		body, _ := in["body"].(string)
		head, _ := in["headRefName"].(string)
		base, _ := in["baseRefName"].(string)
		var pl *Pull
		if err := wrapRest(func() { pl = openPull(r, title, body, head, base) }); err != nil {
			return nil, err
		}
		return J{"pullRequest": pl, "clientMutationId": in["clientMutationId"]}, nil
	}, "input:CreatePullRequestInput!")
	m.f("mergePullRequest", "MergePullRequestPayload", func(_ any, a J) (any, error) {
		in, err := inputOf(a)
		if err != nil {
			return nil, err
		}
		pl, err := nodeOfKind[*Pull](in["pullRequestId"])
		if err != nil {
			return nil, err
		}
		if exp, ok := in["expectedHeadOid"].(string); ok && exp != pullHeadSHA(pl) {
			return nil, ferr("Head branch was modified. Review and try the merge again.")
		}
		method, _ := in["mergeMethod"].(string)
		if method == "" {
			method = "MERGE"
		}
		headline, _ := in["commitHeadline"].(string)
		body, _ := in["commitBody"].(string)
		if _, status, msg := mergePull(pl, strings.ToLower(method), headline, body); status != 200 {
			return nil, ferr(msg)
		}
		return J{"pullRequest": pl, "clientMutationId": in["clientMutationId"]}, nil
	}, "input:MergePullRequestInput!")
	pullState := func(state string) func(any, J) (any, error) {
		return func(_ any, a J) (any, error) {
			in, err := inputOf(a)
			if err != nil {
				return nil, err
			}
			pl, err := nodeOfKind[*Pull](in["pullRequestId"])
			if err != nil {
				return nil, err
			}
			setPullState(pl, state)
			return J{"pullRequest": pl, "clientMutationId": in["clientMutationId"]}, nil
		}
	}
	m.f("closePullRequest", "ClosePullRequestPayload", pullState("closed"), "input:ClosePullRequestInput!")
	m.f("reopenPullRequest", "ReopenPullRequestPayload", pullState("open"), "input:ReopenPullRequestInput!")
	m.f("addComment", "AddCommentPayload", func(_ any, a J) (any, error) {
		in, err := inputOf(a)
		if err != nil {
			return nil, err
		}
		subject := fromGlobalID(in["subjectId"])
		body, _ := in["body"].(string)
		var c *Comment
		var count int
		switch s := subject.(type) {
		case *Issue:
			if body == "" {
				return nil, ferr("Body can't be blank")
			}
			c = addCommentTo(s.Repo, s.Number, &s.Comments, body)
			s.UpdatedAt = c.CreatedAt
			s.json = nil
			count = len(s.Comments)
		case *Pull:
			if body == "" {
				return nil, ferr("Body can't be blank")
			}
			c = addCommentTo(s.Repo, s.Number, &s.Comments, body)
			count = len(s.Comments)
		default:
			return nil, &gqlFieldError{fmt.Sprintf("Could not resolve to a node with the global id of '%v'", in["subjectId"]), "NOT_FOUND"}
		}
		edge := J{"cursor": cursorOf(count - 1), "node": c}
		return J{"commentEdge": edge, "timelineEdge": edge, "subject": subject, "clientMutationId": in["clientMutationId"]}, nil
	}, "input:AddCommentInput!")
}

func anyList(v any) []any {
	l, _ := v.([]any)
	return l
}

func typeOf(v any) string {
	switch x := v.(type) {
	case *Repo:
		return "Repository"
	case *Issue:
		return "Issue"
	case *Pull:
		return "PullRequest"
	case *Comment:
		return "IssueComment"
	case *Label:
		return "Label"
	case *User:
		return "User"
	case J:
		if t, ok := x["__typename"].(string); ok {
			return t
		}
	}
	return "Node"
}

// ------------------------------------------------------------------ introspection

var introCache = map[string]J{}

func introType(ref string) any {
	if strings.HasSuffix(ref, "!") {
		return J{"kind": "NON_NULL", "name": nil, "description": nil, "ofType": introType(ref[:len(ref)-1])}
	}
	if strings.HasPrefix(ref, "[") {
		return J{"kind": "LIST", "name": nil, "description": nil, "ofType": introType(ref[1 : len(ref)-1])}
	}
	if t := introNamed(ref); t != nil {
		return t
	}
	return nil
}

func introArgs(list []string) []J {
	out := []J{}
	for _, a := range list {
		parts := strings.SplitN(a, ":", 2)
		out = append(out, J{"name": parts[0], "description": nil, "type": introType(parts[1]), "defaultValue": nil, "isDeprecated": false, "deprecationReason": nil})
	}
	return out
}

func introNamed(name string) J {
	if c, ok := introCache[name]; ok {
		return c
	}
	def := types[name]
	if def == nil {
		return nil
	}
	out := J{"kind": def.kind, "name": name, "description": nil, "ofType": nil, "specifiedByURL": nil}
	introCache[name] = out
	if len(def.fieldOrder) > 0 || def.kind == "OBJECT" || def.kind == "INTERFACE" {
		fields := []J{}
		for _, fname := range def.fieldOrder {
			fd := def.fields[fname]
			fields = append(fields, J{"name": fname, "description": nil, "args": introArgs(fd.args), "type": introType(fd.typ), "isDeprecated": false, "deprecationReason": nil})
		}
		out["fields"] = fields
	} else {
		out["fields"] = nil
	}
	if def.enumValues != nil {
		vals := []J{}
		for _, v := range def.enumValues {
			vals = append(vals, J{"name": v, "description": nil, "isDeprecated": false, "deprecationReason": nil})
		}
		out["enumValues"] = vals
	} else {
		out["enumValues"] = nil
	}
	if def.kind == "OBJECT" {
		ifaces := []J{}
		for _, i := range def.interfaces {
			ifaces = append(ifaces, introNamed(i))
		}
		out["interfaces"] = ifaces
	} else {
		out["interfaces"] = nil
	}
	if def.possibleTypes != nil {
		pts := []J{}
		for _, p := range def.possibleTypes {
			pts = append(pts, introNamed(p))
		}
		out["possibleTypes"] = pts
	} else {
		out["possibleTypes"] = nil
	}
	if def.inputFields != nil {
		out["inputFields"] = introArgs(def.inputFields)
	} else {
		out["inputFields"] = nil
	}
	return out
}

func schemaIntro() J {
	all := []J{}
	for _, n := range typeOrder {
		all = append(all, introNamed(n))
	}
	ifArg := []J{{"name": "if", "description": nil, "type": introType("Boolean!"), "defaultValue": nil}}
	return J{"description": nil, "queryType": introNamed("Query"), "mutationType": introNamed("Mutation"), "subscriptionType": nil, "types": all,
		"directives": []J{
			{"name": "include", "description": nil, "locations": []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, "args": ifArg, "isRepeatable": false},
			{"name": "skip", "description": nil, "locations": []string{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, "args": ifArg, "isRepeatable": false},
		}}
}

// ------------------------------------------------------------------ execution

type executor struct {
	doc    *gDoc
	vars   J
	errors []any
}

func (x *executor) value(v *gValue) any {
	switch v.kind {
	case "var":
		return x.vars[v.s]
	case "int":
		n, _ := strconv.ParseFloat(v.s, 64)
		return n
	case "float":
		n, _ := strconv.ParseFloat(v.s, 64)
		return n
	case "string", "enum":
		return v.s
	case "bool":
		return v.b
	case "list":
		out := make([]any, len(v.list))
		for i, e := range v.list {
			out[i] = x.value(e)
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range v.fields {
			out[f.name] = x.value(f.val)
		}
		return out
	}
	return nil
}

func (x *executor) included(ds []gDirective) bool {
	for _, d := range ds {
		val := true
		for _, a := range d.args {
			if a.name == "if" {
				b, _ := x.value(a.val).(bool)
				val = b
			}
		}
		if d.name == "skip" && val {
			return false
		}
		if d.name == "include" && !val {
			return false
		}
	}
	return true
}

func applies(cond, typeName string) bool {
	if cond == "" || cond == typeName {
		return true
	}
	def := types[cond]
	if def == nil {
		return false
	}
	if contains(def.possibleTypes, typeName) {
		return true
	}
	if t := types[typeName]; t != nil {
		return contains(t.interfaces, cond)
	}
	return false
}

func namedType(t string) string { return strings.Trim(t, "[]!") }

func (x *executor) validate(typeName string, sels []*gSel, path []any, seen map[string]bool) {
	def := types[typeName]
	for _, s := range sels {
		switch s.kind {
		case "spread":
			fr := x.doc.fragments[s.name]
			if fr == nil {
				x.errors = append(x.errors, J{"message": "Fragment " + s.name + " was used, but not defined", "extensions": J{"code": "undefinedFragment"}})
				continue
			}
			if seen[s.name] {
				continue
			}
			seen[s.name] = true
			on := fr.on
			if types[on] == nil {
				on = typeName
			}
			x.validate(on, fr.sels, path, seen)
		case "inline":
			if s.on != "" && types[s.on] == nil {
				x.errors = append(x.errors, J{"message": "No such type " + s.on + ", so it can't be a fragment condition", "extensions": J{"code": "undefinedType", "typeName": s.on}})
				continue
			}
			on := s.on
			if on == "" {
				on = typeName
			}
			x.validate(on, s.sels, path, seen)
		default:
			if s.name == "__typename" || (typeName == "Query" && (s.name == "__type" || s.name == "__schema")) {
				continue
			}
			var fd *fieldDef
			if def != nil {
				fd = def.fields[s.name]
			}
			if fd == nil {
				p := append(append([]any{}, path...), s.name)
				x.errors = append(x.errors, J{"message": "Field '" + s.name + "' doesn't exist on type '" + typeName + "'", "path": p,
					"extensions": J{"code": "undefinedField", "typeName": typeName, "fieldName": s.name}})
				continue
			}
			if s.hasSels {
				key := s.alias
				if key == "" {
					key = s.name
				}
				x.validate(namedType(fd.typ), s.sels, append(append([]any{}, path...), key), seen)
			}
		}
	}
}

func (x *executor) collect(typeName string, sels []*gSel, into []*gSel) []*gSel {
	for _, s := range sels {
		if !x.included(s.directives) {
			continue
		}
		switch s.kind {
		case "field":
			into = append(into, s)
		case "inline":
			if applies(s.on, typeName) {
				into = x.collect(typeName, s.sels, into)
			}
		case "spread":
			if fr := x.doc.fragments[s.name]; fr != nil && applies(fr.on, typeName) {
				into = x.collect(typeName, fr.sels, into)
			}
		}
	}
	return into
}

func (x *executor) args(s *gSel) J {
	out := J{}
	for _, a := range s.args {
		out[a.name] = x.value(a.val)
	}
	return out
}

func (x *executor) object(typeName string, value any, sels []*gSel, path []any) ordered {
	fields := x.collect(typeName, sels, nil)
	out := make(ordered, 0, len(fields))
	put := func(k string, v any) {
		if prior, i := out.get(k); i >= 0 {
			if po, ok := prior.(ordered); ok {
				if vo, ok := v.(ordered); ok {
					merged := append(ordered{}, po...)
					for _, e := range vo {
						if _, j := merged.get(e.k); j < 0 {
							merged = append(merged, e)
						}
					}
					out[i].v = merged
					return
				}
			}
			out[i].v = v
			return
		}
		out = append(out, kv{k, v})
	}
	for _, s := range fields {
		key := s.alias
		if key == "" {
			key = s.name
		}
		if s.name == "__typename" {
			put(key, typeName)
			continue
		}
		a := x.args(s)
		if typeName == "Query" && s.name == "__type" {
			name, _ := a["name"].(string)
			if t := introNamed(name); t != nil {
				put(key, x.dynamic(t, s.sels))
			} else {
				put(key, nil)
			}
			continue
		}
		if typeName == "Query" && s.name == "__schema" {
			put(key, x.dynamic(schemaIntro(), s.sels))
			continue
		}
		def := types[typeName]
		var fd *fieldDef
		if def != nil {
			fd = def.fields[s.name]
		}
		if fd == nil {
			put(key, nil)
			continue
		}
		fieldPath := append(append([]any{}, path...), key)
		raw, err := fd.resolve(value, a)
		if err != nil {
			fe, ok := err.(*gqlFieldError)
			typ := "UNPROCESSABLE"
			if ok {
				typ = fe.typ
			}
			x.errors = append(x.errors, J{"type": typ, "path": fieldPath, "message": err.Error()})
			put(key, nil)
			continue
		}
		put(key, x.complete(fd.typ, raw, s, fieldPath))
	}
	return out
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case *Repo:
		return x == nil
	case *Issue:
		return x == nil
	case *Pull:
		return x == nil
	case *User:
		return x == nil
	case *Label:
		return x == nil
	case *Comment:
		return x == nil
	case *string:
		return x == nil
	case J:
		return x == nil
	}
	return false
}

func (x *executor) complete(typ string, value any, s *gSel, path []any) any {
	bare := strings.TrimSuffix(typ, "!")
	if isNil(value) {
		return nil
	}
	if strings.HasPrefix(bare, "[") {
		inner := bare[1 : len(bare)-1]
		var list []any
		switch l := value.(type) {
		case []any:
			list = l
		case []J:
			for _, e := range l {
				list = append(list, e)
			}
		default:
			return nil
		}
		out := make([]any, len(list))
		for i, e := range list {
			out[i] = x.complete(inner, e, s, append(append([]any{}, path...), i))
		}
		return out
	}
	if p, ok := value.(*string); ok {
		return *p
	}
	def := types[bare]
	if def == nil || def.kind == "SCALAR" || def.kind == "ENUM" || !s.hasSels {
		return value
	}
	concrete := bare
	if def.kind == "INTERFACE" || def.kind == "UNION" {
		concrete = typeOf(value)
	}
	return x.object(concrete, value, s.sels, path)
}

// dynamic selects properties of plain introspection maps.
func (x *executor) dynamic(value any, sels []*gSel) any {
	switch v := value.(type) {
	case nil:
		return nil
	case []J:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = x.dynamic(e, sels)
		}
		return out
	case []string:
		return v
	case J:
		if v == nil {
			return nil
		}
		typeName := "__Object"
		if _, ok := v["ofType"]; ok {
			typeName = "__Type"
		}
		fields := x.collect(typeName, sels, nil)
		out := make(ordered, 0, len(fields))
		for _, s := range fields {
			key := s.alias
			if key == "" {
				key = s.name
			}
			if s.name == "__typename" {
				out = append(out, kv{key, typeName})
				continue
			}
			item := v[s.name]
			if !s.hasSels {
				out = append(out, kv{key, item})
			} else {
				out = append(out, kv{key, x.dynamic(item, s.sels)})
			}
		}
		return out
	}
	return value
}

func execute(body any) any {
	req, ok := body.(map[string]any)
	if !ok {
		return J{"errors": []any{J{"message": "A query attribute must be specified and must be a string."}}}
	}
	query, _ := req["query"].(string)
	if strings.TrimSpace(query) == "" {
		return J{"errors": []any{J{"message": "A query attribute must be specified and must be a string."}}}
	}
	doc, err := parseDocument(query)
	if err != nil {
		return J{"errors": []any{J{"message": "Parse error: " + err.Error(), "extensions": J{"code": "parseError"}}}}
	}
	opName, _ := req["operationName"].(string)
	var op *gOp
	if opName == "" {
		if len(doc.ops) == 1 {
			op = doc.ops[0]
		}
	} else {
		for _, o := range doc.ops {
			if o.name == opName {
				op = o
			}
		}
	}
	if op == nil {
		msg := "An operation name is required when the document holds more than one operation"
		if opName != "" {
			msg = `No operation named "` + opName + `"`
		}
		return J{"errors": []any{J{"message": msg}}}
	}
	given, _ := req["variables"].(map[string]any)
	vars := J{}
	x := &executor{doc: doc, vars: vars}
	for _, v := range op.vars {
		if val, has := given[v.name]; has {
			vars[v.name] = val
		} else if v.def != nil {
			vars[v.name] = x.value(v.def)
		} else if strings.HasSuffix(v.typ, "!") {
			return J{"errors": []any{J{"message": "Variable $" + v.name + " of type " + v.typ + " was provided invalid value", "extensions": J{"code": "variableMismatch"}}}}
		} else {
			vars[v.name] = nil
		}
	}
	root := "Query"
	if op.typ == "mutation" {
		root = "Mutation"
	}
	label := op.typ
	if op.name != "" {
		label += " " + op.name
	}
	x.validate(root, op.sels, []any{label}, map[string]bool{})
	if len(x.errors) > 0 {
		return J{"errors": x.errors}
	}
	data := x.object(root, nil, op.sels, []any{})
	if len(x.errors) > 0 {
		return ordered{{"data", data}, {"errors", x.errors}}
	}
	return ordered{{"data", data}}
}

func safeExecute(body any) (out any) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[forge] graphql: %v\n", rec)
			out = J{"data": nil, "errors": []any{J{"message": "Something went wrong while executing your query."}}}
		}
	}()
	return execute(body)
}

func handleGraphql(method string, authed bool, raw []byte) (int, any) {
	if !authed {
		return 401, J{"message": "This endpoint requires you to be authenticated.", "documentation_url": "https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql"}
	}
	if method != "POST" {
		return 405, J{"message": "Use POST for GraphQL requests"}
	}
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return 400, J{"message": "Problems parsing JSON"}
	}
	if list, ok := body.([]any); ok {
		out := make([]any, len(list))
		for i, e := range list {
			out[i] = safeExecute(e)
		}
		return 200, out
	}
	return 200, safeExecute(body)
}
