// The forge's in-memory store: repositories (objects, refs, labels, issues, pull requests) and the git
// operations the REST, GraphQL and MCP surfaces share. One global lock (storeMu) serializes handlers.
package main

import (
	"crypto/sha1"
	"encoding/base64"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var storeMu sync.Mutex

var viewerLogin = func() string {
	if v := os.Getenv("MAKEFUN_FORGE_LOGIN"); v != "" {
		return v
	}
	return "adaiversen"
}()

type User struct {
	ID    int
	Login string
	Name  string
	Email string
}

var viewer = &User{ID: 1001, Login: viewerLogin, Name: viewerLogin, Email: viewerLogin + "@users.noreply.forge.local"}

type Label struct {
	ID          int
	Name        string
	Color       string
	Description *string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
	Repo      *Repo
	Number    int
}

type Issue struct {
	ID          int
	Repo        *Repo
	Number      int
	Title       string
	Body        string
	State       string
	StateReason *string
	Labels      []*Label
	Comments    []*Comment
	CreatedAt   string
	UpdatedAt   string
	ClosedAt    *string
	json        []byte
}

type Pull struct {
	ID             int
	Repo           *Repo
	Number         int
	Title          string
	Body           string
	State          string
	Head, Base     string
	HeadSHA        string // frozen once merged or closed
	BaseSHA        string
	MergeBaseSHA   string
	frozen         bool
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       *string
	Comments       []*Comment
}

var nextID = 100000

func newID() int {
	nextID++
	return nextID
}

func nowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

type Repo struct {
	ID            int
	Owner         *User
	Name          string
	Description   *string
	Private       bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	PushedAt      *string
	Objects       map[string]*Obj
	Refs          map[string]string
	Labels        map[string]*Label
	LabelOrder    []*Label
	Issues        map[int]*Issue
	Pulls         map[int]*Pull
	NextNumber    int
	FetchCache    map[string][]byte
	commitCache   map[string]*Commit
}

func (r *Repo) FullName() string { return r.Owner.Login + "/" + r.Name }

func (r *Repo) put(typ string, data []byte, z []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = &Obj{Type: typ, Data: data, Z: z}
	}
	return oid
}

func (r *Repo) setRef(name, oid string) {
	if oid == "" {
		delete(r.Refs, name)
	} else {
		r.Refs[name] = oid
	}
	if len(r.FetchCache) > 0 {
		r.FetchCache = map[string][]byte{}
	}
	now := nowISO()
	r.PushedAt = &now
}

func (r *Repo) branchTip(branch string) (string, bool) {
	oid, ok := r.Refs["refs/heads/"+branch]
	return oid, ok
}

func (r *Repo) commit(oid string) *Commit {
	if c, ok := r.commitCache[oid]; ok {
		return c
	}
	o := r.Objects[oid]
	if o == nil || o.Type != "commit" {
		return nil
	}
	c := parseCommit(o.Data)
	r.commitCache[oid] = c
	return c
}

func (r *Repo) tree(oid string) []TreeEntry {
	o := r.Objects[oid]
	if o == nil || o.Type != "tree" {
		return nil
	}
	return parseTree(o.Data)
}

func (r *Repo) peel(oid string) string {
	cur := oid
	for i := 0; i < 10; i++ {
		o := r.Objects[cur]
		if o == nil || o.Type != "tag" {
			return cur
		}
		cur = parseTag(o.Data).Object
	}
	return cur
}

func (r *Repo) touch() { r.UpdatedAt = nowISO() }

func (r *Repo) sortedRefs() []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ------------------------------------------------------------------ the repository index

var repos = map[string]*Repo{}
var nodes = map[int]any{}

func repoKey(owner, name string) string { return strings.ToLower(owner) + "/" + strings.ToLower(name) }

func findRepo(owner, name string) *Repo {
	r := repos[repoKey(owner, name)]
	if r == nil || !strings.EqualFold(r.Owner.Login, owner) {
		return nil
	}
	return r
}

func createRepo(name string, desc *string, private bool) *Repo {
	key := repoKey(viewer.Login, name)
	if repos[key] != nil {
		return nil
	}
	now := nowISO()
	r := &Repo{ID: newID(), Owner: viewer, Name: name, Description: desc, Private: private, DefaultBranch: "main", CreatedAt: now, UpdatedAt: now,
		Objects: map[string]*Obj{}, Refs: map[string]string{}, Labels: map[string]*Label{}, Issues: map[int]*Issue{}, Pulls: map[int]*Pull{},
		NextNumber: 1, FetchCache: map[string][]byte{}, commitCache: map[string]*Commit{}}
	repos[key] = r
	nodes[r.ID] = r
	return r
}

func deleteRepo(r *Repo) {
	delete(repos, repoKey(r.Owner.Login, r.Name))
	delete(nodes, r.ID)
}

func listRepos() []*Repo {
	out := make([]*Repo, 0, len(repos))
	for _, r := range repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func addIssue(r *Repo, title, body string, labels []*Label) *Issue {
	now := nowISO()
	is := &Issue{ID: newID(), Repo: r, Number: r.NextNumber, Title: title, Body: body, State: "open", Labels: labels, CreatedAt: now, UpdatedAt: now}
	r.NextNumber++
	r.Issues[is.Number] = is
	nodes[is.ID] = is
	r.touch()
	return is
}

func setIssueState(is *Issue, state string) {
	if is.State == state {
		return
	}
	is.State = state
	is.UpdatedAt = nowISO()
	if state == "closed" {
		t := is.UpdatedAt
		is.ClosedAt = &t
		reason := "completed"
		is.StateReason = &reason
	} else {
		is.ClosedAt = nil
		reason := "reopened"
		is.StateReason = &reason
	}
	is.json = nil
}

func addCommentTo(r *Repo, number int, list *[]*Comment, body string) *Comment {
	now := nowISO()
	c := &Comment{ID: newID(), Body: body, CreatedAt: now, UpdatedAt: now, Repo: r, Number: number}
	*list = append(*list, c)
	nodes[c.ID] = c
	return c
}

func addLabel(r *Repo, name, color string, desc *string) *Label {
	if r.Labels[strings.ToLower(name)] != nil {
		return nil
	}
	l := &Label{ID: newID(), Name: name, Color: color, Description: desc}
	r.Labels[strings.ToLower(name)] = l
	r.LabelOrder = append(r.LabelOrder, l)
	nodes[l.ID] = l
	return l
}

// resolveLabels answers the labels named, or the first missing name.
func resolveLabels(r *Repo, names []any) ([]*Label, string, bool) {
	var out []*Label
	for _, raw := range names {
		name := ""
		switch v := raw.(type) {
		case string:
			name = v
		case map[string]any:
			name, _ = v["name"].(string)
		default:
			name = toString(v)
		}
		l := r.Labels[strings.ToLower(name)]
		if l == nil {
			return nil, name, false
		}
		dup := false
		for _, have := range out {
			if have == l {
				dup = true
			}
		}
		if !dup {
			out = append(out, l)
		}
	}
	return out, "", true
}

// ------------------------------------------------------------------ history

func isAncestor(r *Repo, ancestor, desc string) bool {
	if ancestor == desc {
		return true
	}
	seen := map[string]bool{desc: true}
	stack := []string{desc}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c := r.commit(oid)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if p == ancestor {
				return true
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

func ancestors(r *Repo, tip string) map[string]bool {
	seen := map[string]bool{tip: true}
	stack := []string{tip}
	for len(stack) > 0 {
		c := r.commit(stack[len(stack)-1])
		stack = stack[:len(stack)-1]
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return seen
}

func mergeBase(r *Repo, a, b string) string {
	fromA := ancestors(r, a)
	seen := map[string]bool{b: true}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if fromA[oid] {
			return oid
		}
		c := r.commit(oid)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return ""
}

func logFrom(r *Repo, tip string, limit int) []string {
	var out []string
	seen := map[string]bool{tip: true}
	queue := []string{tip}
	for len(queue) > 0 && len(out) < limit {
		oid := queue[0]
		queue = queue[1:]
		c := r.commit(oid)
		if c == nil {
			continue
		}
		out = append(out, oid)
		for _, p := range c.Parents {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return out
}

// ------------------------------------------------------------------ trees

type FlatEntry struct{ OID, Mode string }

func flatten(r *Repo, tree string, prefix string, out map[string]FlatEntry) map[string]FlatEntry {
	if out == nil {
		out = map[string]FlatEntry{}
	}
	if tree == "" {
		return out
	}
	for _, e := range r.tree(tree) {
		path := prefix + e.Name
		if e.Mode == "40000" {
			flatten(r, e.OID, path+"/", out)
		} else {
			out[path] = FlatEntry{e.OID, e.Mode}
		}
	}
	return out
}

func treeFromFlat(r *Repo, flat map[string]FlatEntry) string {
	var files []TreeEntry
	dirs := map[string]map[string]FlatEntry{}
	for path, e := range flat {
		slash := strings.IndexByte(path, '/')
		if slash < 0 {
			files = append(files, TreeEntry{Mode: e.Mode, Name: path, OID: e.OID})
			continue
		}
		d := path[:slash]
		if dirs[d] == nil {
			dirs[d] = map[string]FlatEntry{}
		}
		dirs[d][path[slash+1:]] = e
	}
	for name, child := range dirs {
		files = append(files, TreeEntry{Mode: "40000", Name: name, OID: treeFromFlat(r, child)})
	}
	return r.put("tree", buildTree(files), nil)
}

func lookupPath(r *Repo, tree, path string) *TreeEntry {
	parts := splitPath(path)
	if len(parts) == 0 {
		return &TreeEntry{Mode: "40000", OID: tree}
	}
	cur := tree
	for i, part := range parts {
		var found *TreeEntry
		for _, e := range r.tree(cur) {
			if e.Name == part {
				e := e
				found = &e
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(parts)-1 {
			return found
		}
		if found.Mode != "40000" {
			return nil
		}
		cur = found.OID
	}
	return nil
}

func splitPath(path string) []string {
	var out []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// updateTree sets (blob) or removes ("" blob) one path; "" when the tree becomes empty.
func updateTree(r *Repo, tree string, parts []string, blob string) string {
	var entries []TreeEntry
	if tree != "" {
		entries = append(entries, r.tree(tree)...)
	}
	head := parts[0]
	idx := -1
	for i, e := range entries {
		if e.Name == head {
			idx = i
		}
	}
	var repl *TreeEntry
	if len(parts) == 1 {
		if blob != "" {
			repl = &TreeEntry{Mode: "100644", Name: head, OID: blob}
		}
	} else {
		childTree := ""
		if idx >= 0 && entries[idx].Mode == "40000" {
			childTree = entries[idx].OID
		}
		if child := updateTree(r, childTree, parts[1:], blob); child != "" {
			repl = &TreeEntry{Mode: "40000", Name: head, OID: child}
		}
	}
	if idx >= 0 {
		entries = append(entries[:idx], entries[idx+1:]...)
	}
	if repl != nil {
		entries = append(entries, *repl)
	}
	if len(entries) == 0 {
		return ""
	}
	return r.put("tree", buildTree(entries), nil)
}

var lastStamp int64

func makeCommit(r *Repo, tree string, parents []string, message string) string {
	now := time.Now().Unix()
	if lastStamp+1 > now {
		now = lastStamp + 1
	}
	lastStamp = now
	ident := viewer.Name + " <" + viewer.Email + "> " + strconv.FormatInt(now, 10) + " +0000"
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return r.put("commit", buildCommit(tree, parents, ident, ident, message), nil)
}

// commitFile makes a commit on a branch setting (content non-nil) or deleting one file.
func commitFile(r *Repo, branch, path string, content []byte, message string) (string, string) {
	tip, has := r.branchTip(branch)
	baseTree := ""
	if has {
		if c := r.commit(tip); c != nil {
			baseTree = c.Tree
		}
	}
	blob := ""
	if content != nil {
		blob = r.put("blob", content, nil)
	}
	tree := updateTree(r, baseTree, splitPath(path), blob)
	if tree == "" {
		tree = r.put("tree", []byte{}, nil)
	}
	var parents []string
	if has {
		parents = []string{tip}
	}
	c := makeCommit(r, tree, parents, message)
	r.setRef("refs/heads/"+branch, c)
	return c, blob
}

// ------------------------------------------------------------------ merges and diffs

func mergeTrees(r *Repo, base, ours, theirs string) (string, bool) {
	b := flatten(r, base, "", nil)
	l := flatten(r, ours, "", nil)
	rt := flatten(r, theirs, "", nil)
	out := map[string]FlatEntry{}
	paths := map[string]bool{}
	for p := range b {
		paths[p] = true
	}
	for p := range l {
		paths[p] = true
	}
	for p := range rt {
		paths[p] = true
	}
	for p := range paths {
		be, bok := b[p]
		le, lok := l[p]
		re, rok := rt[p]
		same := func(x FlatEntry, xok bool, y FlatEntry, yok bool) bool {
			if !xok {
				return !yok
			}
			return yok && x == y
		}
		var pick FlatEntry
		var pok bool
		switch {
		case same(le, lok, re, rok):
			pick, pok = le, lok
		case same(be, bok, re, rok):
			pick, pok = le, lok
		case same(be, bok, le, lok):
			pick, pok = re, rok
		default:
			return "", false
		}
		if pok {
			out[p] = pick
		}
	}
	return treeFromFlat(r, out), true
}

func treeOf(r *Repo, oid string) string {
	if oid == "" {
		return ""
	}
	if c := r.commit(oid); c != nil {
		return c.Tree
	}
	return ""
}

type MergePlan struct {
	OK        bool
	Tree      string
	MergeBase string
}

func planMerge(r *Repo, baseTip, headTip string) MergePlan {
	mb := mergeBase(r, baseTip, headTip)
	tree, ok := mergeTrees(r, treeOf(r, mb), treeOf(r, baseTip), treeOf(r, headTip))
	return MergePlan{OK: ok, Tree: tree, MergeBase: mb}
}

type FileChange struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	SHA       string `json:"sha"`
}

func blobLines(r *Repo, oid string) []string {
	if oid == "" {
		return nil
	}
	o := r.Objects[oid]
	if o == nil || len(o.Data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(o.Data), "\n"), "\n")
}

func lineDelta(before, after []string) (int, int) {
	pool := map[string]int{}
	for _, l := range before {
		pool[l]++
	}
	adds := 0
	for _, l := range after {
		if pool[l] > 0 {
			pool[l]--
		} else {
			adds++
		}
	}
	dels := 0
	for _, n := range pool {
		dels += n
	}
	return adds, dels
}

func diffCommits(r *Repo, from, to string) []FileChange {
	before := flatten(r, treeOf(r, from), "", nil)
	after := flatten(r, treeOf(r, to), "", nil)
	set := map[string]bool{}
	for p := range before {
		set[p] = true
	}
	for p := range after {
		set[p] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := []FileChange{}
	for _, p := range paths {
		b, bok := before[p]
		a, aok := after[p]
		if bok && aok && b.OID == a.OID {
			continue
		}
		adds, dels := lineDelta(blobLines(r, b.OID), blobLines(r, a.OID))
		status := "modified"
		sha := a.OID
		if !bok {
			status = "added"
		} else if !aok {
			status = "removed"
			sha = b.OID
		}
		out = append(out, FileChange{Filename: p, Status: status, Additions: adds, Deletions: dels, Changes: adds + dels, SHA: sha})
	}
	return out
}

// ------------------------------------------------------------------ pull requests

func addPull(r *Repo, title, body, head, base string) *Pull {
	now := nowISO()
	p := &Pull{ID: newID(), Repo: r, Number: r.NextNumber, Title: title, Body: body, State: "open", Head: head, Base: base, CreatedAt: now, UpdatedAt: now}
	r.NextNumber++
	r.Pulls[p.Number] = p
	nodes[p.ID] = p
	r.touch()
	return p
}

func pullHeadSHA(p *Pull) string {
	if p.frozen {
		return p.HeadSHA
	}
	tip, _ := p.Repo.branchTip(p.Head)
	return tip
}

func pullBaseSHA(p *Pull) string {
	if p.frozen {
		return p.BaseSHA
	}
	tip, _ := p.Repo.branchTip(p.Base)
	return tip
}

func pullFiles(p *Pull) []FileChange {
	head := pullHeadSHA(p)
	if head == "" {
		return []FileChange{}
	}
	from := p.MergeBaseSHA
	if !p.frozen || from == "" {
		if base := pullBaseSHA(p); base != "" && from == "" {
			from = mergeBase(p.Repo, base, head)
		}
	}
	return diffCommits(p.Repo, from, head)
}

var mergeableCache = map[string]bool{}

// pullMergeable: nil once merged, else whether it merges cleanly.
func pullMergeable(p *Pull) *bool {
	if p.Merged {
		return nil
	}
	head, base := pullHeadSHA(p), pullBaseSHA(p)
	f := false
	if head == "" || base == "" {
		return &f
	}
	key := strconv.Itoa(p.Repo.ID) + ":" + base + ":" + head
	ok, hit := mergeableCache[key]
	if !hit {
		ok = planMerge(p.Repo, base, head).OK
		mergeableCache[key] = ok
	}
	return &ok
}

func mergePull(p *Pull, method, title, message string) (string, int, string) {
	if p.Merged {
		return "", 405, "Pull Request is not mergeable: it is already merged"
	}
	if p.State != "open" {
		return "", 405, "Pull Request is not mergeable: it is closed"
	}
	r := p.Repo
	head, hok := r.branchTip(p.Head)
	base, bok := r.branchTip(p.Base)
	if !hok || !bok {
		return "", 405, "Pull Request is not mergeable: a branch is missing"
	}
	plan := planMerge(r, base, head)
	if !plan.OK {
		return "", 405, "Pull Request is not mergeable"
	}
	if title == "" {
		title = p.Title + " (#" + strconv.Itoa(p.Number) + ")"
	}
	msg := title
	if message != "" {
		msg = title + "\n\n" + message
	}
	parents := []string{base}
	if method == "merge" {
		parents = []string{base, head}
	}
	sha := makeCommit(r, plan.Tree, parents, msg)
	r.setRef("refs/heads/"+p.Base, sha)
	now := nowISO()
	p.HeadSHA, p.BaseSHA, p.MergeBaseSHA, p.frozen = head, base, plan.MergeBase, true
	p.Merged = true
	p.MergedAt = &now
	p.MergeCommitSHA = &sha
	p.State = "closed"
	p.ClosedAt = &now
	p.UpdatedAt = now
	return sha, 200, ""
}

func setPullState(p *Pull, state string) {
	if p.Merged || p.State == state {
		return
	}
	p.State = state
	p.UpdatedAt = nowISO()
	if state == "closed" {
		t := p.UpdatedAt
		p.ClosedAt = &t
		p.HeadSHA, p.BaseSHA = pullHeadSHA(p), pullBaseSHA(p)
		p.frozen = true
	} else {
		p.ClosedAt = nil
		p.frozen = false
		p.HeadSHA, p.BaseSHA = "", ""
	}
}

// ------------------------------------------------------------------ global ids

func globalID(prefix string, id int) string {
	sum := sha1.Sum([]byte("forge:" + prefix + ":" + strconv.Itoa(id)))
	digest := base64.RawURLEncoding.EncodeToString(sum[:])[:6]
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(id)+":"+digest))
}

func globalIDOf(v any) string {
	switch x := v.(type) {
	case *Repo:
		return globalID("R", x.ID)
	case *Issue:
		return globalID("I", x.ID)
	case *Pull:
		return globalID("PR", x.ID)
	case *Comment:
		return globalID("IC", x.ID)
	case *Label:
		return globalID("LA", x.ID)
	case *User:
		return globalID("U", x.ID)
	}
	return ""
}

func fromGlobalID(id any) any {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	under := strings.IndexByte(s, '_')
	if under < 0 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s[under+1:])
	if err != nil {
		return nil
	}
	n, err := strconv.Atoi(strings.SplitN(string(raw), ":", 2)[0])
	if err != nil {
		return nil
	}
	var found any
	if n == viewer.ID {
		found = viewer
	} else {
		found = nodes[n]
	}
	if found == nil || globalIDOf(found) != s {
		return nil
	}
	return found
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case nil:
		return ""
	}
	return ""
}
