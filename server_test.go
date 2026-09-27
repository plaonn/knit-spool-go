package spool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testWSURL(serverURL string) string {
	return "ws" + strings.TrimPrefix(serverURL, "http") + websocketPath
}

func readWireRecord(t *testing.T, c *websocket.Conn) record {
	t.Helper()
	kind, data, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatalf("server sent websocket message type %d", kind)
	}
	r, err := decodeRecord(data, 1<<20)
	if err != nil {
		t.Fatalf("decode server record: %v", err)
	}
	return r
}

func writeWireRecord(t *testing.T, c *websocket.Conn, r record) {
	t.Helper()
	data, err := encodeRecord(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
}

func TestWebSocketHelloSubscribePushAndOperationalRoutes(t *testing.T) {
	cfg := testConfig(t)
	e := newTestEngine(t, cfg)
	s := NewServer(cfg, e)
	httpServer := httptest.NewServer(s.http.Handler)
	defer httpServer.Close()
	ws, _, err := websocket.DefaultDialer.Dial(testWSURL(httpServer.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	hello := readWireRecord(t, ws)
	if hello["t"] != "hello" || hello["v"] != int64(1) {
		t.Fatalf("unexpected server hello: %#v", hello)
	}
	writeWireRecord(t, ws, record{"t": "hello", "v": int64(1)})
	scope := protocolFixture(32, 70)
	subRequest := record{"t": "sub", "q": int64(1), "subs": []any{map[string]any{
		"scope": scope, "bounds": map[string]any{"maxFrames": int64(8), "ttlMs": int64(60_000), "maxBlob": int64(1024)},
	}}}
	writeWireRecord(t, ws, subRequest)
	if got := readWireRecord(t, ws); got["t"] != "digest" {
		t.Fatalf("subscribe reply: %#v", got)
	}
	data := []byte("wire push")
	blob := BlobID(data)
	writeWireRecord(t, ws, record{"t": "push", "q": int64(2), "scope": scope, "blobId": blob[:], "data": data})
	if got := readWireRecord(t, ws); got["t"] != "ok" || got["q"] != int64(2) {
		t.Fatalf("push ack: %#v", got)
	}
	writeWireRecord(t, ws, record{"t": "hello", "v": int64(1)})
	if got := readWireRecord(t, ws); got["t"] != "err" || got["code"] != "malformed" {
		t.Fatalf("post-negotiation hello: %#v", got)
	}
	if response, err := http.Get(httpServer.URL + "/healthz"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v %v", response, err)
	} else {
		response.Body.Close()
	}
	if response, err := http.Get(httpServer.URL + "/source"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("source: %v %v", response, err)
	} else {
		response.Body.Close()
	}
	if response, err := http.Get(httpServer.URL + "/metrics"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("metrics: %v %v", response, err)
	} else {
		response.Body.Close()
	}
}

func TestAuthenticationCapacityAndPreHelloCloseCodes(t *testing.T) {
	cfg := testConfig(t)
	cfg.Token = "correct horse battery staple"
	cfg.MaxConns = 1
	cfg.MaxConnsPerIP = 10
	e := newTestEngine(t, cfg)
	s := NewServer(cfg, e)
	httpServer := httptest.NewServer(s.http.Handler)
	defer httpServer.Close()
	bad, _, err := websocket.DefaultDialer.Dial(testWSURL(httpServer.URL), nil)
	if err != nil {
		t.Fatalf("unauthenticated connection should receive close after upgrade: %v", err)
	}
	_, _, err = bad.ReadMessage()
	if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != closeAuth {
		t.Fatalf("unauthenticated close: %#v", err)
	}

	url := testWSURL(httpServer.URL) + "?k=" + strings.ReplaceAll(cfg.Token, " ", "%20")
	first, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWireRecord(t, first); got["t"] != "hello" {
		t.Fatalf("authenticated hello: %#v", got)
	}
	response, err := http.Get(httpServer.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("metrics without token status %d", response.StatusCode)
	}
	response.Body.Close()
	response, err = http.Get(httpServer.URL + "/metrics?k=" + strings.ReplaceAll(cfg.Token, " ", "%20"))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metrics with token status %d", response.StatusCode)
	}
	response.Body.Close()

	second, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		second.Close()
		t.Fatal("second connection exceeded the configured cap")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("capacity response: %#v %v", resp, err)
	}
	first.Close()

	// On a separate uncapped listener, a pre-HELLO text frame closes with 4000.
	cfg.MaxConns = 0
	e2 := newTestEngine(t, cfg)
	s2 := NewServer(cfg, e2)
	httpServer2 := httptest.NewServer(s2.http.Handler)
	defer httpServer2.Close()
	pre, _, err := websocket.DefaultDialer.Dial(testWSURL(httpServer2.URL)+"?k="+strings.ReplaceAll(cfg.Token, " ", "%20"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = readWireRecord(t, pre)
	if err := pre.WriteMessage(websocket.TextMessage, []byte("not binary")); err != nil {
		t.Fatal(err)
	}
	_, _, err = pre.ReadMessage()
	if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != closeMalformed {
		t.Fatalf("pre-hello close: %#v", err)
	}
}

func TestInvalidTokenDoesNotReceiveHello(t *testing.T) {
	cfg := testConfig(t)
	cfg.Token = "expected"
	e := newTestEngine(t, cfg)
	s := NewServer(cfg, e)
	httpServer := httptest.NewServer(s.http.Handler)
	defer httpServer.Close()
	c, _, err := websocket.DefaultDialer.Dial(testWSURL(httpServer.URL)+"?k=wrong", nil)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _, err = c.ReadMessage()
	if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != closeAuth {
		t.Fatalf("invalid-token response: %#v", err)
	}
}

func TestCapacityRefusalLeavesHealthAvailable(t *testing.T) {
	cfg := testConfig(t)
	cfg.MaxConns = 1
	e := newTestEngine(t, cfg)
	s := NewServer(cfg, e)
	httpServer := httptest.NewServer(s.http.Handler)
	defer httpServer.Close()
	ws, _, err := websocket.DefaultDialer.Dial(testWSURL(httpServer.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = readWireRecord(t, ws)
	response, err := http.Get(httpServer.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthz during capacity: %d", response.StatusCode)
	}
	response.Body.Close()
}
