package hub

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Assets are the files the hub serves: the page and the agent install scripts.
type Assets struct {
	Web     fs.FS // index.html, app.js, style.css, fonts/...
	Scripts fs.FS // install.ps1, uninstall.ps1, install.sh, uninstall.sh, btop-agent.service
}

type staticFile struct {
	body, gz []byte
	etag     string
	ctype    string
}

// Server wires the hub to HTTP.
type Server struct {
	hub     *Hub
	allow   []netip.Prefix
	static  map[string]*staticFile
	scripts fs.FS

	distMu   sync.Mutex
	distHash map[string]string

	rejectMu  sync.Mutex
	rejectLog map[string]time.Time
}

// NewServer prepares the handler. allow lists the source networks that may connect;
// loopback is always allowed so the container's health check works.
func NewServer(h *Hub, allow []netip.Prefix, a Assets) (*Server, error) {
	s := &Server{hub: h, allow: allow, static: map[string]*staticFile{}, scripts: a.Scripts,
		distHash: map[string]string{}, rejectLog: map[string]time.Time{}}
	err := fs.WalkDir(a.Web, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(p, ".go") {
			return err
		}
		b, err := fs.ReadFile(a.Web, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		f := &staticFile{body: b, etag: `"` + hex.EncodeToString(sum[:8]) + `"`, ctype: mime.TypeByExtension(path.Ext(p))}
		if f.ctype == "" {
			f.ctype = "application/octet-stream"
		}
		if strings.HasSuffix(p, ".woff2") {
			f.ctype = "font/woff2"
		} else if f.ctype != "font/woff2" {
			var gz bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
			zw.Write(b)
			zw.Close()
			if gz.Len() < len(b) {
				f.gz = gz.Bytes()
			}
		}
		s.static["/"+p] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.static["/index.html"] == nil {
		return nil, fmt.Errorf("web assets missing index.html")
	}
	s.static["/"] = s.static["/index.html"]
	return s, nil
}

func (s *Server) allowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return true
	}
	for _, p := range s.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// ServeHTTP filters by source address, adds security headers and routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.allowed(r.RemoteAddr) {
		s.logReject(r.RemoteAddr)
		w.Header().Set("Connection", "close")
		http.Error(w, "forbidden: this hub only answers its own network", http.StatusForbidden)
		return
	}
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
	hdr.Set("X-Frame-Options", "DENY")

	p := r.URL.Path
	switch {
	case p == "/healthz":
		io.WriteString(w, "ok\n")
	case p == "/api/history" && r.Method == http.MethodGet:
		s.history(w, r)
	case p == "/api/stream" && r.Method == http.MethodGet:
		s.hub.stream(w, r)
	case p == "/api/agent/stream":
		s.hub.handleAgentStream(w, r)
	case p == "/api/agent/check":
		if !s.hub.authorized(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		io.WriteString(w, "ok\n")
	case strings.HasPrefix(p, "/api/machines/") && r.Method == http.MethodDelete:
		if !s.hub.authorized(r) {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		if !s.hub.Forget(strings.TrimPrefix(p, "/api/machines/")) {
			http.Error(w, "no such machine", http.StatusNotFound)
			return
		}
		io.WriteString(w, "forgotten\n")
	case p == "/api/stats":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.hub.Stats())
	case p == "/install.ps1" || p == "/uninstall.ps1" || p == "/install.sh" || p == "/uninstall.sh" || p == "/btop-agent.service":
		s.script(w, r, strings.TrimPrefix(p, "/"))
	case strings.HasPrefix(p, "/dist/"):
		s.dist(w, r, strings.TrimPrefix(p, "/dist/"))
	default:
		s.serveStatic(w, r)
	}
}

func (s *Server) logReject(remote string) {
	host, _, _ := net.SplitHostPort(remote)
	s.rejectMu.Lock()
	defer s.rejectMu.Unlock()
	if t, ok := s.rejectLog[host]; ok && time.Since(t) < 10*time.Minute {
		return
	}
	if len(s.rejectLog) > 1000 {
		s.rejectLog = map[string]time.Time{}
	}
	s.rejectLog[host] = time.Now()
	log.Printf("rejected connection from %s (not in the allowed networks)", host)
}

func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f := s.static[r.URL.Path]
	if f == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", "no-cache")
	if f.ctype == "text/html; charset=utf-8" {
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	}
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.body
	if f.gz != nil {
		h.Set("Vary", "Accept-Encoding")
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.Set("Content-Encoding", "gzip")
			body = f.gz
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	var out io.Writer = w
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer zw.Close()
		out = zw
	}
	if err := s.hub.writeHistory(out, q.Get("m"), after); err != nil {
		log.Printf("history: %v", err)
	}
}

// publicURL is the configured hub URL, or the one this request used.
func (s *Server) publicURL(r *http.Request) string {
	if u := s.hub.cfg.PublicURL; u != "" {
		return strings.TrimRight(u, "/")
	}
	host := r.Host
	if strings.ContainsAny(host, " \"'`$;&|<>\\") || host == "" {
		host = "localhost"
	}
	return "http://" + host
}

// openDist opens an agent binary. The image stores them gzipped (to stay small) and the
// hub decompresses while serving, so scripts download and verify the plain file.
func (s *Server) openDist(name string) (io.ReadCloser, int64, time.Time, error) {
	p := filepath.Join(s.hub.cfg.DistDir, name)
	if f, err := os.Open(p); err == nil {
		fi, _ := f.Stat()
		return f, fi.Size(), fi.ModTime(), nil
	}
	f, err := os.Open(p + ".gz")
	if err != nil {
		return nil, 0, time.Time{}, err
	}
	fi, _ := f.Stat()
	var size int64 = -1
	var trailer [4]byte // gzip ISIZE: uncompressed length mod 2^32
	if _, err := f.ReadAt(trailer[:], fi.Size()-4); err == nil {
		size = int64(binary.LittleEndian.Uint32(trailer[:]))
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, 0, time.Time{}, err
	}
	return struct {
		io.Reader
		io.Closer
	}{zr, f}, size, fi.ModTime(), nil
}

// distSHA is the SHA-256 of the served (decompressed) binary, cached until the file changes.
func (s *Server) distSHA(name string) (string, error) {
	s.distMu.Lock()
	defer s.distMu.Unlock()
	rc, _, mod, err := s.openDist(name)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	key := name + "@" + mod.String()
	if h, ok := s.distHash[key]; ok {
		return h, nil
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, rc); err != nil {
		return "", err
	}
	h := hex.EncodeToString(sum.Sum(nil))
	s.distHash[key] = h
	return h, nil
}

// script serves an install script with the hub URL and the agent's checksum filled in.
func (s *Server) script(w http.ResponseWriter, r *http.Request, name string) {
	b, err := fs.ReadFile(s.scripts, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	bin := "btop-agent.exe"
	if strings.HasSuffix(name, ".sh") || strings.HasSuffix(name, ".service") {
		bin = "btop-agent-linux-amd64"
	}
	sum, err := s.distSHA(bin)
	if err != nil {
		sum = "missing-agent-binary"
	}
	out := strings.NewReplacer("__HUB_URL__", s.publicURL(r), "__AGENT_SHA256__", sum).Replace(string(b))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, out)
}

func (s *Server) dist(w http.ResponseWriter, r *http.Request, name string) {
	if name != "btop-agent.exe" && name != "btop-agent-linux-amd64" {
		http.NotFound(w, r)
		return
	}
	rc, size, mod, err := s.openDist(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Last-Modified", mod.UTC().Format(http.TimeFormat))
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodHead {
		return
	}
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	io.Copy(w, rc)
}

// ParseAllow parses "192.168.1.0/24,10.0.0.0/8".
func ParseAllow(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		p, err := netip.ParsePrefix(part)
		if err != nil {
			return nil, fmt.Errorf("HUB_ALLOW %q: %w", part, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
