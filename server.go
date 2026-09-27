package spool

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	websocketPath  = "/spool/v1"
	closeMalformed = 4000
	closeAuth      = 4001
	closeVersion   = 4002
	closeAbuse     = 4003
)

// Server exposes the protocol engine over the v1 WebSocket and the small HTTP ops surface.
type Server struct {
	cfg      Config
	engine   *Engine
	http     *http.Server
	mu       sync.Mutex
	live     int
	byIP     map[string]int
	active   map[*websocket.Conn]struct{}
	connWG   sync.WaitGroup
	draining bool
	upgrader websocket.Upgrader
}

// NewServer constructs an HTTP/WebSocket server. TLS is enabled by Run when configured.
func NewServer(cfg Config, engine *Engine) *Server {
	s := &Server{cfg: cfg, engine: engine, byIP: make(map[string]int), active: make(map[*websocket.Conn]struct{})}
	s.upgrader = websocket.Upgrader{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		EnableCompression: false,
		CheckOrigin:       func(*http.Request) bool { return true },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/source", s.source)
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc(websocketPath, s.websocket)
	s.http = &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 * 1024,
	}
	return s
}

// Run serves until ctx is cancelled. It returns the listener or shutdown error.
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	serve := make(chan error, 1)
	go func() {
		if s.cfg.TLSCertFile != "" {
			serve <- s.http.ServeTLS(listener, s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		} else {
			serve <- s.http.Serve(listener)
		}
	}()
	select {
	case err := <-serve:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		s.closeActive()
		s.connWG.Wait()
		return err
	case <-ctx.Done():
		s.mu.Lock()
		s.draining = true
		s.mu.Unlock()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			_ = s.http.Close()
			s.closeActive()
			s.connWG.Wait()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		s.closeActive()
		s.connWG.Wait()
		err := <-serve
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.engine.totalBytes(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "fail"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) source(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	version, commit := "dev", "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		version = info.Main.Version
		if version == "" || version == "(devel)" {
			version = "dev"
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				commit = setting.Value
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"name": "knit-spool-go", "version": version, "commit": commit,
		"source": s.cfg.SourceURL, "license": "AGPL-3.0-or-later",
	})
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.acceptMetrics(r.URL.Query().Get("k")) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	stats := s.engine.Stats()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics := []struct {
		name     string
		typeName string
		help     string
	}{
		{"connections", "gauge", "Current WebSocket connections."},
		{"connections_total", "counter", "WebSocket connections accepted since process start."},
		{"records", "counter", "Protocol records handled since process start."},
		{"pushes", "counter", "Frame pushes stored since process start."},
		{"events", "counter", "Frame events delivered to subscribers since process start."},
		{"pow_verified", "counter", "Proof-of-work challenges verified since process start."},
		{"rate_limited", "counter", "Requests refused by rate limits since process start."},
		{"scopes_shed", "counter", "Scopes evicted by the storage watermark since process start."},
		{"chunks_stored", "counter", "Attachment chunks stored since process start."},
		{"egress_bytes", "counter", "Encoded bytes queued to WebSocket clients since process start."},
		{"scopes", "gauge", "Scopes currently retained."},
		{"live_bytes", "gauge", "Stored frame and attachment payload bytes."},
	}
	for _, metric := range metrics {
		fmt.Fprintf(w, "# HELP knit_spool_%s %s\n# TYPE knit_spool_%s %s\n", metric.name, metric.help, metric.name, metric.typeName)
	}
	for _, key := range []string{"connections", "connections_total", "records", "pushes", "events", "pow_verified", "rate_limited", "scopes_shed", "chunks_stored", "egress_bytes", "scopes", "live_bytes"} {
		fmt.Fprintf(w, "knit_spool_%s %d\n", key, stats[key])
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) acceptMetrics(candidate string) bool {
	if s.cfg.MetricsToken != "" {
		return sameSecret(candidate, s.cfg.MetricsToken)
	}
	if s.cfg.Token == "" && s.cfg.TokenNext == "" {
		return true
	}
	return sameSecret(candidate, s.cfg.Token) || sameSecret(candidate, s.cfg.TokenNext)
}

func (s *Server) acceptConnect(candidate string) bool {
	if s.cfg.Token == "" && s.cfg.TokenNext == "" {
		return true
	}
	return sameSecret(candidate, s.cfg.Token) || sameSecret(candidate, s.cfg.TokenNext)
}

func sameSecret(candidate, expected string) bool {
	if expected == "" {
		return false
	}
	a := sha256.Sum256([]byte(candidate))
	b := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != websocketPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	remote, err := normalizeAddress(r.RemoteAddr, s.cfg.TrustProxy, r.Header.Get("X-Forwarded-For"))
	if err != nil {
		http.Error(w, "bad client address", http.StatusBadRequest)
		return
	}
	if !s.reserve() {
		w.Header().Set("Retry-After", "5")
		http.Error(w, "at capacity", http.StatusServiceUnavailable)
		return
	}
	ipCounted := false
	defer func() {
		if ipCounted {
			s.release(remote)
		} else {
			s.releaseSlot()
		}
	}()
	c, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(int64(s.cfg.MaxRecord))
	c.SetReadDeadline(time.Now().Add(90 * time.Second))
	c.SetPongHandler(func(string) error { return c.SetReadDeadline(time.Now().Add(90 * time.Second)) })
	if !s.withinIPLimit(remote) {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeAbuse, "too many connections"), time.Now().Add(time.Second))
		_ = c.Close()
		return
	}
	ipCounted = true
	if !s.acceptConnect(r.URL.Query().Get("k")) {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(closeAuth, "auth"), time.Now().Add(time.Second))
		_ = c.Close()
		return
	}
	if !s.trackConn(c) {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "going away"), time.Now().Add(time.Second))
		_ = c.Close()
		return
	}
	defer s.untrackConn(c)
	peer := s.engine.Register(remote)
	s.serveConnection(c, peer)
	s.engine.Unregister(peer)
}

func (s *Server) reserve() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining || (s.cfg.MaxConns > 0 && s.live >= s.cfg.MaxConns) {
		return false
	}
	s.live++
	return true
}

func (s *Server) trackConn(c *websocket.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return false
	}
	s.active[c] = struct{}{}
	s.connWG.Add(1)
	return true
}

func (s *Server) untrackConn(c *websocket.Conn) {
	s.mu.Lock()
	delete(s.active, c)
	s.mu.Unlock()
	s.connWG.Done()
}

func (s *Server) closeActive() {
	s.mu.Lock()
	active := make([]*websocket.Conn, 0, len(s.active))
	for c := range s.active {
		active = append(active, c)
	}
	s.mu.Unlock()
	for _, c := range active {
		_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1001, "going away"), time.Now().Add(time.Second))
		_ = c.Close()
	}
}

func (s *Server) withinIPLimit(remote string) bool {
	key := connectionIPKey(remote)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byIP[key]++
	if s.cfg.MaxConnsPerIP > 0 && s.byIP[key] > s.cfg.MaxConnsPerIP {
		s.byIP[key]--
		return false
	}
	return true
}

func (s *Server) release(remote string) {
	key := connectionIPKey(remote)
	s.mu.Lock()
	if s.byIP[key] > 0 {
		s.byIP[key]--
		if s.byIP[key] == 0 {
			delete(s.byIP, key)
		}
	}
	if s.live > 0 {
		s.live--
	}
	s.mu.Unlock()
}

func (s *Server) releaseSlot() {
	s.mu.Lock()
	if s.live > 0 {
		s.live--
	}
	s.mu.Unlock()
}

func connectionIPKey(address string) string {
	ip := net.ParseIP(address)
	if ip == nil {
		return address
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}

func (s *Server) serveConnection(c *websocket.Conn, peer *Peer) {
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); s.writeLoop(c, peer) }()
	peer.send(s.engine.Hello())
	negotiated := false
	for {
		messageType, payload, err := c.ReadMessage()
		if err != nil {
			break
		}
		if len(payload) > s.cfg.MaxRecord {
			if !negotiated {
				s.closeConn(c, closeMalformed, "record too large")
				break
			}
			s.engine.HandleMalformed(peer, "too_large")
			continue
		}
		if messageType != websocket.BinaryMessage {
			if !negotiated {
				s.closeConn(c, closeMalformed, "hello first")
				break
			}
			s.engine.HandleMalformed(peer, "malformed")
			continue
		}
		record, err := decodeRecord(payload, s.cfg.MaxRecord)
		if err != nil {
			if !negotiated {
				s.closeConn(c, closeMalformed, "hello first")
				break
			}
			s.engine.HandleMalformed(peer, "malformed")
			continue
		}
		if !negotiated {
			if mustRecordType(record) != "hello" {
				s.closeConn(c, closeMalformed, "hello first")
				break
			}
			v, ok := record.integer("v")
			if !ok {
				s.closeConn(c, closeMalformed, "malformed hello")
				break
			}
			minimum := int64(1)
			if _, exists := record["min"]; exists {
				value, valid := record.integer("min")
				if !valid {
					s.closeConn(c, closeMalformed, "malformed hello")
					break
				}
				minimum = value
			}
			if minimum < 1 || minimum > v || v < 1 || v > ProtocolVersion {
				s.closeConn(c, closeVersion, "no version overlap")
				break
			}
			negotiated = true
			continue
		}
		if mustRecordType(record) == "hello" {
			s.engine.Handle(peer, record)
			continue
		}
		s.engine.Handle(peer, record)
		select {
		case <-peer.closed:
			goto done
		default:
		}
	}
done:
	peer.close(1000)
	_ = c.Close()
	<-writerDone
}

func (s *Server) closeConn(c *websocket.Conn, code int, reason string) {
	_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	_ = c.Close()
}

func (s *Server) writeLoop(c *websocket.Conn, peer *Peer) {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-peer.closed:
			code := 1000
			select {
			case code = <-peer.closeCode:
			default:
			}
			for {
				select {
				case r := <-peer.out:
					if !s.writeRecord(c, peer, r) {
						return
					}
				default:
					_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, ""), time.Now().Add(time.Second))
					_ = c.Close()
					return
				}
			}
		case r := <-peer.out:
			if !s.writeRecord(c, peer, r) {
				return
			}
		case <-ping.C:
			if err := c.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				peer.close(1011)
				return
			}
		}
	}
}

func (s *Server) writeRecord(c *websocket.Conn, peer *Peer, r record) bool {
	data, err := encodeRecord(r)
	if err != nil {
		peer.close(1011)
		return false
	}
	_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.WriteMessage(websocket.BinaryMessage, data); err != nil {
		peer.close(1011)
		return false
	}
	s.engine.counters.EgressBytes.Add(int64(len(data)))
	return true
}
