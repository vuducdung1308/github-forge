// Git object model, packfiles and pkt-lines on the standard library only.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ZeroOID = "0000000000000000000000000000000000000000"

// Obj is one stored object; Z caches its deflated bytes so packs are written without deflating twice.
type Obj struct {
	Type string
	Data []byte
	Z    []byte
}

var typeNum = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var numType = [8]string{"", "commit", "tree", "blob", "tag", "", "", ""}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write(strconv.AppendInt(nil, int64(len(data)), 10))
	h.Write([]byte{0})
	h.Write(data)
	var sum [20]byte
	h.Sum(sum[:0])
	return hex.EncodeToString(sum[:])
}

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------------ commits, trees, tags

type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func parseCommit(data []byte) *Commit {
	text := string(data)
	c := &Commit{}
	head := text
	if i := strings.Index(text, "\n\n"); i >= 0 {
		head = text[:i]
		c.Message = text[i+2:]
	}
	for _, line := range strings.Split(head, "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		key, val := line[:sp], line[sp+1:]
		switch key {
		case "tree":
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author = val
		case "committer":
			c.Committer = val
		}
	}
	return c
}

func buildCommit(tree string, parents []string, author, committer, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + author + "\ncommitter " + committer + "\n\n" + message)
	return []byte(b.String())
}

type Ident struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

func parseIdent(s string) Ident {
	lt := strings.LastIndex(s, " <")
	gt := strings.LastIndex(s, "> ")
	if lt < 0 || gt < lt {
		return Ident{Name: s, Date: isoSeconds(0)}
	}
	rest := strings.Fields(s[gt+2:])
	var secs int64
	if len(rest) > 0 {
		secs, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	return Ident{Name: s[:lt], Email: s[lt+2 : gt], Date: isoSeconds(secs)}
}

func isoSeconds(secs int64) string {
	return time.Unix(secs, 0).UTC().Format("2006-01-02T15:04:05Z")
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			break
		}
		nul := bytes.IndexByte(data[pos+sp:], 0)
		if nul < 0 {
			break
		}
		nul += pos + sp
		if nul+21 > len(data) {
			break
		}
		out = append(out, TreeEntry{Mode: string(data[pos : pos+sp]), Name: string(data[pos+sp+1 : nul]), OID: hex.EncodeToString(data[nul+1 : nul+21])})
		pos = nul + 21
	}
	return out
}

func treeKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return treeKey(sorted[i]) < treeKey(sorted[j]) })
	var b bytes.Buffer
	for _, e := range sorted {
		b.WriteString(e.Mode + " " + e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		b.Write(raw)
	}
	return b.Bytes()
}

type Tag struct {
	Object, Type, Name string
}

func parseTag(data []byte) Tag {
	var t Tag
	text := string(data)
	if i := strings.Index(text, "\n\n"); i >= 0 {
		text = text[:i]
	}
	for _, line := range strings.Split(text, "\n") {
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		switch line[:sp] {
		case "object":
			t.Object = line[sp+1:]
		case "type":
			t.Type = line[sp+1:]
		case "tag":
			t.Name = line[sp+1:]
		}
	}
	return t
}

// ------------------------------------------------------------------ deltas

func readDeltaSize(d []byte, pos int) (int, int, error) {
	v, shift := 0, 0
	for {
		if pos >= len(d) {
			return 0, 0, errors.New("truncated delta header")
		}
		b := d[pos]
		pos++
		v |= int(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			return v, pos, nil
		}
	}
}

func applyDelta(base, delta []byte) ([]byte, error) {
	src, pos, err := readDeltaSize(delta, 0)
	if err != nil {
		return nil, err
	}
	if src != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	dst, pos, err := readDeltaSize(delta, pos)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, dst)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<i) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					off |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if pos >= len(delta) {
						return nil, errors.New("truncated delta copy")
					}
					size |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op > 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("delta insert out of range")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != dst {
		return nil, errors.New("delta result size mismatch")
	}
	return out, nil
}

// ------------------------------------------------------------------ packs

type packEntry struct {
	offset   int
	kind     byte
	raw      []byte
	z        []byte
	baseOff  int
	baseOID  string
	resolved *Obj
}

var inflaterPool sync.Pool

// inflate decompresses one zlib stream at the start of src, reporting how many bytes it used.
func inflate(src []byte) ([]byte, int, error) {
	br := bytes.NewReader(src)
	var zr io.ReadCloser
	if pooled, ok := inflaterPool.Get().(io.ReadCloser); ok {
		if err := pooled.(zlib.Resetter).Reset(br, nil); err != nil {
			return nil, 0, err
		}
		zr = pooled
	} else {
		r, err := zlib.NewReader(br)
		if err != nil {
			return nil, 0, err
		}
		zr = r
	}
	out, err := io.ReadAll(zr)
	inflaterPool.Put(zr)
	if err != nil {
		return nil, 0, err
	}
	return out, len(src) - br.Len(), nil
}

// readPack reads a packfile into objects, resolving ofs and ref deltas (a ref base may come from lookup).
func readPack(pack []byte, lookup func(string) *Obj) (map[string]*Obj, error) {
	objects := map[string]*Obj{}
	if len(pack) == 0 {
		return objects, nil
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("not a packfile")
	}
	version := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack version %d", version)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	body := pack[:len(pack)-20]
	entries := make([]*packEntry, 0, count)
	byOffset := map[int]*packEntry{}
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= len(body) {
			return nil, errors.New("pack truncated")
		}
		e := &packEntry{offset: pos}
		b := body[pos]
		pos++
		e.kind = (b >> 4) & 7
		for b&0x80 != 0 {
			if pos >= len(body) {
				return nil, errors.New("pack truncated")
			}
			b = body[pos]
			pos++
		}
		switch e.kind {
		case 6:
			b = body[pos]
			pos++
			rel := int(b & 0x7f)
			for b&0x80 != 0 {
				b = body[pos]
				pos++
				rel = (rel+1)<<7 | int(b&0x7f)
			}
			e.baseOff = e.offset - rel
		case 7:
			if pos+20 > len(body) {
				return nil, errors.New("pack truncated")
			}
			e.baseOID = hex.EncodeToString(body[pos : pos+20])
			pos += 20
		case 1, 2, 3, 4:
		default:
			return nil, fmt.Errorf("bad pack object type %d", e.kind)
		}
		raw, used, err := inflate(body[pos:])
		if err != nil {
			return nil, fmt.Errorf("zlib: %v", err)
		}
		e.raw = raw
		if e.kind <= 4 {
			e.z = append([]byte(nil), body[pos:pos+used]...)
		}
		pos += used
		entries = append(entries, e)
		byOffset[e.offset] = e
	}
	byOID := map[string]*Obj{}
	var resolve func(e *packEntry, depth int) (*Obj, error)
	resolve = func(e *packEntry, depth int) (*Obj, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if depth > 4000 {
			return nil, errors.New("delta chain too deep")
		}
		var o *Obj
		switch e.kind {
		case 6:
			base := byOffset[e.baseOff]
			if base == nil {
				return nil, errors.New("ofs-delta base not in pack")
			}
			bo, err := resolve(base, depth+1)
			if err != nil || bo == nil {
				return bo, err
			}
			data, err := applyDelta(bo.Data, e.raw)
			if err != nil {
				return nil, err
			}
			o = &Obj{Type: bo.Type, Data: data}
		case 7:
			bo := byOID[e.baseOID]
			if bo == nil {
				bo = lookup(e.baseOID)
			}
			if bo == nil {
				return nil, nil
			}
			data, err := applyDelta(bo.Data, e.raw)
			if err != nil {
				return nil, err
			}
			o = &Obj{Type: bo.Type, Data: data}
		default:
			o = &Obj{Type: numType[e.kind], Data: e.raw, Z: e.z}
		}
		e.resolved = o
		oid := hashObject(o.Type, o.Data)
		byOID[oid] = o
		if _, ok := objects[oid]; !ok {
			objects[oid] = o
		}
		return o, nil
	}
	pending := entries
	for len(pending) > 0 {
		var next []*packEntry
		for _, e := range pending {
			o, err := resolve(e, 0)
			if err != nil {
				return nil, err
			}
			if o == nil {
				next = append(next, e)
			}
		}
		if len(next) == len(pending) {
			return nil, errors.New("pack holds a delta whose base is missing")
		}
		pending = next
	}
	return objects, nil
}

func packHeader(typ string, size int) []byte {
	var out []byte
	b := typeNum[typ]<<4 | byte(size&0x0f)
	rest := size >> 4
	for rest > 0 {
		out = append(out, b|0x80)
		b = byte(rest & 0x7f)
		rest >>= 7
	}
	return append(out, b)
}

var deflaterPool = sync.Pool{New: func() any {
	w, _ := zlib.NewWriterLevel(io.Discard, zlib.BestSpeed)
	return w
}}

func deflated(o *Obj) []byte {
	if o.Z == nil {
		var buf bytes.Buffer
		w := deflaterPool.Get().(*zlib.Writer)
		w.Reset(&buf)
		w.Write(o.Data)
		w.Close()
		deflaterPool.Put(w)
		o.Z = buf.Bytes()
	}
	return o.Z
}

func writePack(objs []*Obj) []byte {
	var b bytes.Buffer
	b.WriteString("PACK")
	n := len(objs)
	b.Write([]byte{0, 0, 0, 2, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	for _, o := range objs {
		b.Write(packHeader(o.Type, len(o.Data)))
		b.Write(deflated(o))
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// ------------------------------------------------------------------ pkt-lines

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

func pkt(dst *bytes.Buffer, payload string) {
	dst.WriteString(fmt.Sprintf("%04x", len(payload)+4))
	dst.WriteString(payload)
}

type Packet struct {
	Kind    byte // 'd' data, 'f' flush, 'l' delim, 'e' end
	Payload []byte
}

// readPackets reads pkt-lines from start until a flush (included, when stopAtFlush) or the end.
func readPackets(buf []byte, start int, stopAtFlush bool) ([]Packet, int, error) {
	var out []Packet
	pos := start
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, errors.New("truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(buf[pos:pos+4]), 16, 16)
		if err != nil {
			return nil, 0, errors.New("bad pkt-line length")
		}
		switch {
		case n == 0:
			out = append(out, Packet{Kind: 'f'})
			pos += 4
			if stopAtFlush {
				return out, pos, nil
			}
			continue
		case n == 1:
			out = append(out, Packet{Kind: 'l'})
			pos += 4
			continue
		case n == 2:
			out = append(out, Packet{Kind: 'e'})
			pos += 4
			continue
		case n < 4 || pos+int(n) > len(buf):
			return nil, 0, errors.New("truncated pkt-line")
		}
		out = append(out, Packet{Kind: 'd', Payload: buf[pos+4 : pos+int(n)]})
		pos += int(n)
	}
	return out, pos, nil
}

func lineOf(p Packet) (string, bool) {
	if p.Kind != 'd' {
		return "", false
	}
	return strings.TrimSuffix(string(p.Payload), "\n"), true
}

// sideband frames data on a band, each packet at most 65520 bytes on the wire.
func sideband(dst *bytes.Buffer, band byte, data []byte) {
	const max = 65515
	for pos := 0; pos < len(data); pos += max {
		end := pos + max
		if end > len(data) {
			end = len(data)
		}
		dst.WriteString(fmt.Sprintf("%04x", end-pos+5))
		dst.WriteByte(band)
		dst.Write(data[pos:end])
	}
}
