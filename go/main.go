// The forge as one static binary: a raw HTTP/1.1 server routing to git smart HTTP, REST v3, GraphQL v4
// and MCP, plus the trial lifecycle (register the port with the env, poll /done, exit 0).
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

var token = os.Getenv("MAKEFUN_SHOP_TOKEN")
var bearer = "Bearer " + token

const jsonType = "application/json; charset=utf-8"
const scopes = "repo, read:org, workflow, gist"

var requestCounter uint64
var idPrefix = fmt.Sprintf("F0RG:%X:", os.Getpid())

type request struct {
	method  string
	target  string
	headers map[string]string
	body    []byte
}

type response struct {
	status  int
	headers [][2]string
	body    []byte
}

func credential(h string) string {
	if h == "" {
		return ""
	}
	scheme, value, _ := strings.Cut(h, " ")
	value = strings.TrimSpace(value)
	switch strings.ToLower(scheme) {
	case "bearer", "token":
		return value
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return ""
		}
		if _, pass, ok := strings.Cut(string(raw), ":"); ok {
			return pass
		}
		return string(raw)
	}
	return ""
}

func authorized(req *request) bool {
	h := req.headers["authorization"]
	if token != "" && h == bearer {
		return true
	}
	c := credential(h)
	return c != "" && (token == "" || c == token)
}

func nextRequestID() string {
	requestCounter++
	return idPrefix + strconv.FormatUint(requestCounter, 16)
}

func jsonResp(status int, body any, extra ...[2]string) response {
	var raw []byte
	switch b := body.(type) {
	case []byte:
		raw = b
	case nil:
	default:
		raw, _ = json.Marshal(b)
	}
	h := append([][2]string{{"Content-Type", jsonType}, {"X-GitHub-Request-Id", nextRequestID()}}, extra...)
	if status == 204 {
		raw = nil
	}
	return response{status, h, raw}
}

func textResp(status int, msg string, extra ...[2]string) response {
	h := append([][2]string{{"Content-Type", "text/plain; charset=utf-8"}, {"X-GitHub-Request-Id", nextRequestID()}}, extra...)
	return response{status, h, []byte(msg)}
}

func baseOf(req *request) string {
	host := req.headers["host"]
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

func splitTarget(t string) (string, string) {
	path, query, _ := strings.Cut(t, "?")
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		rest := path[strings.Index(path, "//")+2:]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			path = rest[i:]
		} else {
			path = "/"
		}
	}
	return path, query
}

func handleAPI(req *request, path, query string) response {
	if !authorized(req) {
		return jsonResp(401, errorBody(401, "Requires authentication", nil), [2]string{"WWW-Authenticate", `Bearer realm="forge"`})
	}
	var body any
	if len(req.body) > 0 {
		if err := json.Unmarshal(req.body, &body); err != nil {
			return jsonResp(400, errorBody(400, "Problems parsing JSON", nil))
		}
	}
	sub := path[7:]
	q := url.Values{}
	if query != "" {
		q, _ = url.ParseQuery(query)
	}
	method := req.method
	if method == "HEAD" {
		method = "GET"
	}
	reply := handleRest(&restReq{method: method, path: sub, query: q, body: body, base: baseOf(req)})
	var extra [][2]string
	for k, v := range reply.headers {
		extra = append(extra, [2]string{k, v})
	}
	if sub == "" || sub == "/" {
		extra = append(extra, [2]string{"X-OAuth-Scopes", scopes})
	}
	if reply.raw != nil {
		return jsonResp(reply.status, reply.raw, extra...)
	}
	if reply.status == 204 {
		return jsonResp(204, nil, extra...)
	}
	return jsonResp(reply.status, reply.body, extra...)
}

var gitPath = regexp.MustCompile(`^/([A-Za-z0-9._-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

func handleGit(req *request, m []string, query string) response {
	if !authorized(req) {
		return textResp(401, "Authentication required\n", [2]string{"WWW-Authenticate", `Basic realm="forge"`})
	}
	r := findRepo(m[1], m[2])
	if r == nil {
		return textResp(404, "Repository not found\n")
	}
	body := req.body
	if strings.EqualFold(req.headers["content-encoding"], "gzip") {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			body, _ = io.ReadAll(zr)
		}
	}
	var reply gitReply
	switch {
	case m[3] == "info/refs" && (req.method == "GET" || req.method == "HEAD"):
		q, _ := url.ParseQuery(query)
		service := q.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return textResp(403, "dumb protocol not supported\n")
		}
		reply = infoRefs(r, service, strings.Contains(req.headers["git-protocol"], "version=2"))
	case m[3] == "git-upload-pack" && req.method == "POST":
		reply = uploadPack(r, body)
	case m[3] == "git-receive-pack" && req.method == "POST":
		reply = receivePack(r, body)
	default:
		return textResp(405, "Method Not Allowed\n")
	}
	return response{reply.status, [][2]string{{"Content-Type", reply.ctype}, {"Cache-Control", "no-cache"}}, reply.body}
}

func route(req *request) (resp response) {
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "[forge] %s %s: %v\n%s\n", req.method, req.target, rec, debug.Stack())
			resp = jsonResp(500, J{"message": "Internal Server Error"})
		}
	}()
	path, query := splitTarget(req.target)
	if path == "/healthz" {
		return jsonResp(200, []byte(`{"ok":true}`))
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	if strings.HasPrefix(path, "/api/v3") && (len(path) == 7 || path[7] == '/') {
		return handleAPI(req, path, query)
	}
	if path == "/api/graphql" || path == "/graphql" {
		status, body := handleGraphql(req.method, authorized(req), req.body)
		return jsonResp(status, body)
	}
	if path == "/mcp" || path == "/mcp/" {
		status, body, headers := handleMcp(req.method, authorized(req), req.body, baseOf(req))
		var extra [][2]string
		for k, v := range headers {
			extra = append(extra, [2]string{k, v})
		}
		if body == nil {
			return response{status, extra, nil}
		}
		return jsonResp(status, body, extra...)
	}
	if m := gitPath.FindStringSubmatch(path); m != nil {
		return handleGit(req, m, query)
	}
	return jsonResp(404, errorBody(404, "Not Found", nil))
}

// ------------------------------------------------------------------ HTTP/1.1

var reasons = map[int]string{100: "Continue", 200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 400: "Bad Request", 401: "Unauthorized",
	403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict", 413: "Payload Too Large", 422: "Unprocessable Entity",
	431: "Request Header Fields Too Large", 500: "Internal Server Error"}

func writeResponse(w *bufio.Writer, resp response, head, close bool) error {
	reason := reasons[resp.status]
	if reason == "" {
		reason = "Status"
	}
	w.WriteString("HTTP/1.1 ")
	w.WriteString(strconv.Itoa(resp.status))
	w.WriteByte(' ')
	w.WriteString(reason)
	w.WriteString("\r\n")
	for _, h := range resp.headers {
		w.WriteString(h[0])
		w.WriteString(": ")
		w.WriteString(h[1])
		w.WriteString("\r\n")
	}
	if resp.status != 204 && resp.status != 304 {
		w.WriteString("Content-Length: ")
		w.WriteString(strconv.Itoa(len(resp.body)))
		w.WriteString("\r\n")
	}
	if close {
		w.WriteString("Connection: close\r\n")
	}
	w.WriteString("\r\n")
	if !head && resp.status != 204 {
		w.Write(resp.body)
	}
	return w.Flush()
}

const maxBody = 256 << 20

func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if err == bufio.ErrBufferFull {
		return "", err
	}
	if err != nil {
		return "", err
	}
	n := len(line) - 1
	if n > 0 && line[n-1] == '\r' {
		n--
	}
	return string(line[:n]), nil
}

func readChunked(r *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		sizeText, _, _ := strings.Cut(line, ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 || int64(len(out))+size > maxBody {
			return nil, fmt.Errorf("bad chunk size")
		}
		if size == 0 {
			for {
				t, err := readLine(r)
				if err != nil {
					return nil, err
				}
				if t == "" {
					return out, nil
				}
			}
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if _, err := readLine(r); err != nil {
			return nil, err
		}
	}
}

func serveConn(c net.Conn) {
	defer c.Close()
	if tc, ok := c.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	r := bufio.NewReaderSize(c, 64<<10)
	w := bufio.NewWriterSize(c, 64<<10)
	badRequest := func(status int) {
		writeResponse(w, jsonResp(status, []byte(`{"message":"`+reasons[status]+`"}`)), false, true)
	}
	for {
		line, err := readLine(r)
		if err != nil {
			if err == bufio.ErrBufferFull {
				badRequest(431)
			}
			return
		}
		if line == "" {
			continue // tolerate a stray CRLF between requests
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
			badRequest(400)
			return
		}
		req := &request{method: parts[0], target: parts[1], headers: make(map[string]string, 8)}
		headerBytes := 0
		for {
			h, err := readLine(r)
			if err != nil {
				return
			}
			if h == "" {
				break
			}
			headerBytes += len(h)
			if headerBytes > 64<<10 {
				badRequest(431)
				return
			}
			name, value, ok := strings.Cut(h, ":")
			if !ok {
				continue
			}
			name = strings.ToLower(strings.TrimSpace(name))
			value = strings.TrimSpace(value)
			if prev, has := req.headers[name]; has {
				value = prev + ", " + value
			}
			req.headers[name] = value
		}
		if strings.EqualFold(req.headers["expect"], "100-continue") {
			w.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
			w.Flush()
		}
		if te := req.headers["transfer-encoding"]; strings.Contains(strings.ToLower(te), "chunked") {
			body, err := readChunked(r)
			if err != nil {
				badRequest(400)
				return
			}
			req.body = body
		} else if cl := req.headers["content-length"]; cl != "" {
			n, err := strconv.ParseInt(cl, 10, 64)
			if err != nil || n < 0 || n > maxBody {
				badRequest(400)
				return
			}
			req.body = make([]byte, n)
			if _, err := io.ReadFull(r, req.body); err != nil {
				return
			}
		}
		conn := strings.ToLower(req.headers["connection"])
		closeAfter := strings.Contains(conn, "close") || (parts[2] == "HTTP/1.0" && !strings.Contains(conn, "keep-alive"))
		resp := route(req)
		if err := writeResponse(w, resp, req.method == "HEAD", closeAfter); err != nil || closeAfter {
			return
		}
	}
}

// ------------------------------------------------------------------ lifecycle

func register(env string, port int) {
	client := &http.Client{Timeout: 30 * time.Second}
	body := []byte(`{"port":` + strconv.Itoa(port) + `}`)
	for attempt := 0; ; attempt++ {
		resp, err := client.Post(env+"/register", "application/json", bytes.NewReader(body))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				fmt.Fprintf(os.Stderr, "[forge] register answered HTTP %d\n", resp.StatusCode)
				os.Exit(1)
			}
		}
		delay := 100 * time.Millisecond << min(attempt, 4)
		time.Sleep(min(delay, time.Second))
	}
}

func waitForDone(env string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(env + "/done")
		if err == nil {
			var v struct {
				Done bool `json:"done"`
			}
			ok := resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&v) == nil
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if ok && v.Done {
				return
			}
		}
		delay := 250 * time.Millisecond << min(attempt, 3)
		time.Sleep(min(delay, 2*time.Second))
	}
}

func main() {
	env := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	addr := ":0"
	if env == "" && os.Getenv("FORGE_PORT") != "" {
		addr = ":" + os.Getenv("FORGE_PORT")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			go serveConn(c)
		}
	}()
	if env == "" {
		select {}
	}
	register(env, port)
	fmt.Println("[forge] registered; waiting for the env to say done")
	waitForDone(env)
	fmt.Println("[forge] done")
	os.Exit(0)
}
