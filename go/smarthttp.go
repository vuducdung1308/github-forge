// Git smart HTTP: info/refs, upload-pack (protocol v2, with a v0 fallback) and receive-pack (v0).
package main

import (
	"bytes"
	"strconv"
	"strings"
)

type gitReply struct {
	status int
	ctype  string
	body   []byte
}

const gitAgent = "agent=forge/2.0"

func gitText(status int, msg string) gitReply {
	return gitReply{status, "text/plain; charset=utf-8", []byte(msg + "\n")}
}

var v2Advert = func() []byte {
	var b bytes.Buffer
	for _, line := range []string{"version 2\n", gitAgent + "\n", "ls-refs\n", "fetch=shallow\n", "server-option\n", "object-format=sha1\n"} {
		pkt(&b, line)
	}
	b.Write(flushPkt)
	return b.Bytes()
}()

func headTarget(r *Repo) string { return "refs/heads/" + r.DefaultBranch }

func infoRefs(r *Repo, service string, v2 bool) gitReply {
	if service == "git-upload-pack" && v2 {
		return gitReply{200, "application/x-git-upload-pack-advertisement", v2Advert}
	}
	var b bytes.Buffer
	pkt(&b, "# service="+service+"\n")
	b.Write(flushPkt)
	var caps string
	if service == "git-receive-pack" {
		caps = "report-status delete-refs side-band-64k quiet ofs-delta " + gitAgent + " object-format=sha1"
	} else {
		caps = "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want symref=HEAD:" + headTarget(r) + " " + gitAgent + " object-format=sha1"
	}
	var lines []string
	if service == "git-upload-pack" {
		if head, ok := r.Refs[headTarget(r)]; ok {
			lines = append(lines, head+" HEAD")
		}
	}
	for _, name := range r.sortedRefs() {
		oid := r.Refs[name]
		lines = append(lines, oid+" "+name)
		if service == "git-upload-pack" && strings.HasPrefix(name, "refs/tags/") {
			if peeled := r.peel(oid); peeled != oid {
				lines = append(lines, peeled+" "+name+"^{}")
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, ZeroOID+" capabilities^{}")
	}
	for i, line := range lines {
		if i == 0 {
			pkt(&b, line+"\x00"+caps+"\n")
		} else {
			pkt(&b, line+"\n")
		}
	}
	b.Write(flushPkt)
	return gitReply{200, "application/x-" + service + "-advertisement", b.Bytes()}
}

// ------------------------------------------------------------------ upload-pack

func uploadPack(r *Repo, body []byte) gitReply {
	packets, _, err := readPackets(body, 0, false)
	if err != nil {
		return gitText(400, "bad request: "+err.Error())
	}
	if len(packets) > 0 {
		if first, ok := lineOf(packets[0]); ok && strings.HasPrefix(first, "command=") {
			return uploadPackV2(r, first[8:], packets)
		}
	}
	return uploadPackV0(r, packets)
}

func uploadPackV2(r *Repo, command string, packets []Packet) gitReply {
	var args []string
	inArgs := false
	for _, p := range packets {
		if p.Kind == 'l' {
			inArgs = true
			continue
		}
		if !inArgs {
			continue
		}
		if line, ok := lineOf(p); ok {
			args = append(args, line)
		}
	}
	switch command {
	case "ls-refs":
		return lsRefs(r, args)
	case "fetch":
		return fetchV2(r, args)
	}
	return gitText(400, "unknown command "+command)
}

func lsRefs(r *Repo, args []string) gitReply {
	symrefs, peel, unborn := false, false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "symrefs":
			symrefs = true
		case a == "peel":
			peel = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[11:])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	target := headTarget(r)
	if keep("HEAD") {
		if head, ok := r.Refs[target]; ok {
			line := head + " HEAD"
			if symrefs {
				line += " symref-target:" + target
			}
			pkt(&b, line+"\n")
		} else if unborn && symrefs {
			pkt(&b, "unborn HEAD symref-target:"+target+"\n")
		}
	}
	for _, name := range r.sortedRefs() {
		if !keep(name) {
			continue
		}
		oid := r.Refs[name]
		line := oid + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if peeled := r.peel(oid); peeled != oid {
				line += " peeled:" + peeled
			}
		}
		pkt(&b, line+"\n")
	}
	b.Write(flushPkt)
	return gitReply{200, "application/x-git-upload-pack-result", b.Bytes()}
}

type fetchArgs struct {
	wants, haves, shallow []string
	depth                 int
	done                  bool
}

func parseFetchArgs(lines []string) (*fetchArgs, string) {
	a := &fetchArgs{}
	for _, raw := range lines {
		word, value := raw, ""
		if sp := strings.IndexByte(raw, ' '); sp >= 0 {
			word = raw[:sp]
			value = strings.SplitN(raw[sp+1:], " ", 2)[0]
		}
		switch word {
		case "want":
			if !isOID(value) {
				return nil, "bad want " + value
			}
			a.wants = append(a.wants, value)
		case "have":
			if isOID(value) {
				a.haves = append(a.haves, value)
			}
		case "shallow":
			if isOID(value) {
				a.shallow = append(a.shallow, value)
			}
		case "deepen":
			d, err := strconv.Atoi(value)
			if err != nil || d < 1 {
				return nil, "bad deepen " + value
			}
			a.depth = d
		case "done":
			a.done = true
		}
	}
	return a, ""
}

type packPlan struct {
	objects            []*Obj
	shallow, unshallow []string
	common             []string
}

func planFetch(r *Repo, a *fetchArgs) (*packPlan, string) {
	clientShallow := map[string]bool{}
	for _, s := range a.shallow {
		clientShallow[s] = true
	}
	held := map[string]bool{}
	var holdTree func(string)
	holdTree = func(oid string) {
		if held[oid] {
			return
		}
		held[oid] = true
		if o := r.Objects[oid]; o == nil || o.Type != "tree" {
			return
		}
		for _, e := range r.tree(oid) {
			if e.Mode != "160000" {
				holdTree(e.OID)
			}
		}
	}
	plan := &packPlan{}
	var stack []string
	for _, h := range a.haves {
		if r.Objects[h] != nil {
			plan.common = append(plan.common, h)
			stack = append(stack, h)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if held[oid] {
			continue
		}
		o := r.Objects[oid]
		if o == nil {
			continue
		}
		switch o.Type {
		case "tag":
			held[oid] = true
			stack = append(stack, parseTag(o.Data).Object)
		case "commit":
			held[oid] = true
			c := r.commit(oid)
			holdTree(c.Tree)
			if !clientShallow[oid] {
				stack = append(stack, c.Parents...)
			}
		case "tree":
			holdTree(oid)
		default:
			held[oid] = true
		}
	}

	sent := map[string]bool{}
	sendObj := func(oid string) {
		if sent[oid] || held[oid] {
			return
		}
		if o := r.Objects[oid]; o != nil {
			sent[oid] = true
			plan.objects = append(plan.objects, o)
		}
	}
	var sendTree func(string)
	sendTree = func(oid string) {
		if sent[oid] || held[oid] {
			return
		}
		sendObj(oid)
		for _, e := range r.tree(oid) {
			switch e.Mode {
			case "160000":
			case "40000":
				sendTree(e.OID)
			default:
				sendObj(e.OID)
			}
		}
	}

	var commitWants []string
	for _, want := range a.wants {
		oid := want
		for i := 0; i < 10; i++ {
			o := r.Objects[oid]
			if o == nil {
				return nil, "not our ref " + want
			}
			if o.Type != "tag" {
				break
			}
			sendObj(oid)
			oid = parseTag(o.Data).Object
		}
		o := r.Objects[oid]
		if o == nil {
			return nil, "not our ref " + want
		}
		switch o.Type {
		case "commit":
			commitWants = append(commitWants, oid)
		case "tree":
			sendTree(oid)
		default:
			sendObj(oid)
		}
	}

	depthOf := map[string]int{}
	var frontier []string
	for _, oid := range commitWants {
		if _, ok := depthOf[oid]; !ok {
			depthOf[oid] = 1
			frontier = append(frontier, oid)
		}
	}
	var commits []string
	newShallow := map[string]bool{}
	level := 1
	for len(frontier) > 0 {
		var next []string
		for _, oid := range frontier {
			c := r.commit(oid)
			if c == nil {
				continue
			}
			atBoundary := a.depth > 0 && level >= a.depth
			if held[oid] && !(a.depth > 0 && clientShallow[oid]) {
				continue
			}
			commits = append(commits, oid)
			if atBoundary {
				if len(c.Parents) > 0 {
					newShallow[oid] = true
					plan.shallow = append(plan.shallow, oid)
				}
				continue
			}
			for _, p := range c.Parents {
				if _, ok := depthOf[p]; ok {
					continue
				}
				depthOf[p] = level + 1
				next = append(next, p)
			}
		}
		frontier = next
		level++
	}
	if a.depth > 0 {
		for _, s := range a.shallow {
			if _, ok := depthOf[s]; ok && !newShallow[s] {
				plan.unshallow = append(plan.unshallow, s)
			}
		}
	} else {
		plan.shallow = nil
	}
	for _, oid := range commits {
		c := r.commit(oid)
		sendObj(oid)
		sendTree(c.Tree)
	}
	return plan, ""
}

func fetchV2(r *Repo, lines []string) gitReply {
	a, bad := parseFetchArgs(lines)
	if a == nil {
		return gitText(400, bad)
	}
	key := ""
	if len(a.haves) == 0 && len(a.shallow) == 0 {
		key = "v2:" + strconv.FormatBool(a.done) + ":" + strconv.Itoa(a.depth) + ":" + strings.Join(a.wants, ",")
		if cached, ok := r.FetchCache[key]; ok {
			return gitReply{200, "application/x-git-upload-pack-result", cached}
		}
	}
	plan, why := planFetch(r, a)
	var b bytes.Buffer
	if plan == nil {
		pkt(&b, "ERR "+why+"\n")
		b.Write(flushPkt)
		return gitReply{200, "application/x-git-upload-pack-result", b.Bytes()}
	}
	if !a.done {
		pkt(&b, "acknowledgments\n")
		if len(plan.common) == 0 {
			pkt(&b, "NAK\n")
		}
		for _, oid := range plan.common {
			pkt(&b, "ACK "+oid+"\n")
		}
		pkt(&b, "ready\n")
		b.Write(delimPkt)
	}
	if a.depth > 0 {
		pkt(&b, "shallow-info\n")
		for _, oid := range plan.shallow {
			pkt(&b, "shallow "+oid+"\n")
		}
		for _, oid := range plan.unshallow {
			pkt(&b, "unshallow "+oid+"\n")
		}
		b.Write(delimPkt)
	}
	pkt(&b, "packfile\n")
	sideband(&b, 1, writePack(plan.objects))
	b.Write(flushPkt)
	out := b.Bytes()
	if key != "" {
		r.FetchCache[key] = out
	}
	return gitReply{200, "application/x-git-upload-pack-result", out}
}

func uploadPackV0(r *Repo, packets []Packet) gitReply {
	var lines []string
	for _, p := range packets {
		if line, ok := lineOf(p); ok {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return gitText(400, "empty upload-pack request")
	}
	a, bad := parseFetchArgs(lines)
	if a == nil {
		return gitText(400, bad)
	}
	plan, why := planFetch(r, a)
	if plan == nil {
		return gitText(400, why)
	}
	var b bytes.Buffer
	if a.depth > 0 {
		for _, oid := range plan.shallow {
			pkt(&b, "shallow "+oid+"\n")
		}
		for _, oid := range plan.unshallow {
			pkt(&b, "unshallow "+oid+"\n")
		}
		b.Write(flushPkt)
	}
	if n := len(plan.common); n > 0 {
		pkt(&b, "ACK "+plan.common[n-1]+"\n")
	} else {
		pkt(&b, "NAK\n")
	}
	sideband(&b, 1, writePack(plan.objects))
	b.Write(flushPkt)
	return gitReply{200, "application/x-git-upload-pack-result", b.Bytes()}
}

// ------------------------------------------------------------------ receive-pack

type pushCommand struct{ old, new, ref string }

func receivePack(r *Repo, body []byte) gitReply {
	packets, next, err := readPackets(body, 0, true)
	if err != nil {
		return gitText(400, "bad request: "+err.Error())
	}
	var cmds []pushCommand
	caps := map[string]bool{}
	for _, p := range packets {
		if p.Kind != 'd' {
			continue
		}
		line := strings.TrimSuffix(string(p.Payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			for _, c := range strings.Fields(line[nul+1:]) {
				caps[c] = true
			}
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 || !isOID(parts[0]) || !isOID(parts[1]) || !strings.HasPrefix(parts[2], "refs/") {
			return gitText(400, "bad command")
		}
		cmds = append(cmds, pushCommand{parts[0], parts[1], parts[2]})
	}
	if len(cmds) == 0 {
		return gitText(400, "no commands")
	}
	unpack := "ok"
	if incoming := body[next:]; len(incoming) > 0 {
		objs, err := readPack(incoming, func(oid string) *Obj { return r.Objects[oid] })
		if err != nil {
			unpack = "error " + err.Error()
		} else {
			for oid, o := range objs {
				if _, ok := r.Objects[oid]; !ok {
					r.Objects[oid] = o
				}
			}
		}
	}
	var report bytes.Buffer
	pkt(&report, "unpack "+unpack+"\n")
	hadBranches := false
	for name := range r.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			hadBranches = true
			break
		}
	}
	firstNew := ""
	for _, c := range cmds {
		if why := applyCommand(r, c, unpack == "ok"); why == "" {
			pkt(&report, "ok "+c.ref+"\n")
			if c.new != ZeroOID && c.old == ZeroOID && strings.HasPrefix(c.ref, "refs/heads/") && firstNew == "" {
				firstNew = c.ref[11:]
			}
		} else {
			pkt(&report, "ng "+c.ref+" "+why+"\n")
		}
	}
	if !hadBranches && firstNew != "" {
		if _, ok := r.branchTip(r.DefaultBranch); !ok {
			r.DefaultBranch = firstNew
		}
	}
	report.Write(flushPkt)
	if !caps["report-status"] && !caps["report-status-v2"] {
		return gitReply{200, "application/x-git-receive-pack-result", nil}
	}
	if caps["side-band-64k"] || caps["side-band"] {
		var out bytes.Buffer
		sideband(&out, 1, report.Bytes())
		out.Write(flushPkt)
		return gitReply{200, "application/x-git-receive-pack-result", out.Bytes()}
	}
	return gitReply{200, "application/x-git-receive-pack-result", report.Bytes()}
}

func applyCommand(r *Repo, c pushCommand, unpacked bool) string {
	current, exists := r.Refs[c.ref]
	if !exists {
		current = ZeroOID
	}
	if c.new == ZeroOID {
		if !exists {
			return "no such ref"
		}
		if c.old != ZeroOID && c.old != current {
			return "stale info"
		}
		r.setRef(c.ref, "")
		return ""
	}
	if !unpacked {
		return "unpacker error"
	}
	if r.Objects[c.new] == nil {
		return "missing necessary objects"
	}
	if strings.HasPrefix(c.ref, "refs/heads/") {
		if o := r.Objects[r.peel(c.new)]; o == nil || o.Type != "commit" {
			return "not a commit"
		}
	}
	if !exists {
		if c.old != ZeroOID {
			return "stale info"
		}
		r.setRef(c.ref, c.new)
		return ""
	}
	if strings.HasPrefix(c.ref, "refs/tags/") {
		return "already exists"
	}
	if c.old != current {
		if isAncestor(r, current, c.new) {
			return "stale info"
		}
		return "non-fast-forward"
	}
	if !isAncestor(r, current, c.new) {
		return "non-fast-forward"
	}
	r.setRef(c.ref, c.new)
	return ""
}
