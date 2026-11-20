// Package e2e drives the built knit-spool binary the way a Knit client does: it mints a relay invite
// link with `knit-spool commons-invite`, starts `knit-spool run` behind TLS, and then uses nothing but
// the link to reach the spool and talk in its commons room.
//
// The client side deliberately avoids the spool package. It decodes the link, derives the room's scope
// id and HELLO tag from the specification's formulas, and speaks CBOR through fxamacker/cbor directly,
// so a mistake shared by the daemon's encoder and its own decoder cannot hide here.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"github.com/gorilla/websocket"
)

// Values from Knit docs/RELAY_INVITE.md (layout 1) and docs/SPOOL_PROTOCOL.md §7.4.
const (
	linkPrefix      = "https://getknit.app/r#"
	commonsDomain   = "knit/spool/v1/commons"
	commonsTagLabel = "knit/spool/v1/commons-tag"
)

// The token holds characters a URL query must escape, so the link's `?k=` has to round-trip them.
const (
	e2eToken = "e2e token/+&=?"
	e2eName  = "E2E 광장"
)

var decMode = func() cbor.DecMode {
	m, err := cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any{})}.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

func TestRelayInviteLinkReachesRunningSpool(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the knit-spool binary")
	}
	binary := buildSpool(t)
	certFile, keyFile, roots := selfSignedCert(t)
	addr := freeLoopbackAddr(t)
	publicURL := "wss://" + addr + "/spool/v1"
	dataDir := t.TempDir()

	env := []string{
		"SPOOL_PUBLIC_URL=" + publicURL,
		"SPOOL_TOKEN=" + e2eToken,
		"SPOOL_COMMONS_NAME=" + e2eName,
	}
	out := runCommand(t, binary, env, "commons-invite")
	link := field(t, out, "link: ")
	commonsID := field(t, out, "SPOOL_COMMONS_ID=")

	startSpool(t, binary, append(env,
		"SPOOL_LISTEN="+addr,
		"SPOOL_TLS_CERT="+certFile,
		"SPOOL_TLS_KEY="+keyFile,
		"SPOOL_DATA_DIR="+dataDir,
		"SPOOL_COMMONS_ID="+commonsID,
		"SPOOL_SWEEP_MS=0",
		"SPOOL_STATUS_MS=0",
	), roots, addr)

	// Everything below uses only the link, as a newcomer's client would.
	invite := decodeLink(t, link)
	if invite.n != e2eName {
		t.Fatalf("link room name = %q, want %q", invite.n, e2eName)
	}
	scope := sha256.Sum256(append([]byte(commonsDomain), invite.c...))
	tagInput := append([]byte(commonsTagLabel), scope[:]...)
	tagSum := sha256.Sum256(tagInput)
	wantTag := tagSum[:8]
	if got := fmt.Sprintf("%x", scope); got != strings.ToLower(commonsID) {
		t.Fatalf("scope derived from the link %s differs from SPOOL_COMMONS_ID %s", got, commonsID)
	}

	dialer := &websocket.Dialer{TLSClientConfig: &tls.Config{RootCAs: roots}, HandshakeTimeout: 5 * time.Second}

	// The link's token is what admits the holder: the same URL without `k` gets no HELLO, only the
	// auth close 4001 (S-7.1-3).
	bare, err := url.Parse(invite.u)
	if err != nil {
		t.Fatal(err)
	}
	if bare.Query().Get("k") != e2eToken {
		t.Fatalf("link URL token = %q, want %q", bare.Query().Get("k"), e2eToken)
	}
	bare.RawQuery = ""
	if conn, resp, err := dialer.Dial(bare.String(), nil); err != nil {
		t.Fatalf("tokenless dial: %v (%v)", err, resp)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := conn.ReadMessage()
		conn.Close()
		if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != 4001 {
			t.Fatalf("tokenless client: want close 4001 before any HELLO, got %v", err)
		}
	}

	alice := join(t, dialer, invite.u, wantTag)
	bob := join(t, dialer, invite.u, wantTag)

	subscribe := map[string]any{"t": "sub", "q": 1, "subs": []any{map[string]any{
		"scope":  scope[:],
		"bounds": map[string]any{"maxFrames": 1, "ttlMs": 1, "maxBlob": 1},
	}}}
	alice.write(t, subscribe)
	alice.readUntil(t, func(r map[string]any) bool { return r["t"] == "digest" })
	bob.write(t, subscribe)
	bob.readUntil(t, func(r map[string]any) bool { return r["t"] == "digest" })

	post := []byte("hello from the e2e test")
	blob := sha256.Sum256(post)
	bob.write(t, map[string]any{"t": "push", "q": 2, "scope": scope[:], "blobId": blob[:], "data": post})
	bob.readUntil(t, func(r map[string]any) bool {
		if r["t"] == "err" {
			t.Fatalf("commons push refused: %v", r)
		}
		return r["t"] == "ok" && r["q"] == uint64(2)
	})
	event := alice.readUntil(t, func(r map[string]any) bool { return r["t"] == "event" })
	if !bytes.Equal(asBytes(event["scope"]), scope[:]) || !bytes.Equal(asBytes(event["data"]), post) ||
		!bytes.Equal(asBytes(event["blobId"]), blob[:]) {
		t.Fatalf("event does not carry bob's post: %v", event)
	}
}

type relayInvite struct {
	u string
	c []byte
	n string
}

func decodeLink(t *testing.T, link string) relayInvite {
	t.Helper()
	if !strings.HasPrefix(link, linkPrefix) {
		t.Fatalf("link %q does not start with %s", link, linkPrefix)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(link, linkPrefix))
	if err != nil {
		t.Fatalf("link fragment is not unpadded base64url: %v", err)
	}
	if len(body) > 512 {
		t.Fatalf("link body is %d bytes, over 512", len(body))
	}
	var fields map[string]any
	if err := decMode.Unmarshal(body, &fields); err != nil {
		t.Fatalf("link body is not a CBOR map: %v", err)
	}
	if fields["v"] != uint64(1) {
		t.Fatalf("link layout version = %v, want 1", fields["v"])
	}
	u, _ := fields["u"].(string)
	c, _ := fields["c"].([]byte)
	n, _ := fields["n"].(string)
	if !strings.HasPrefix(u, "wss://") || len(u) > 256 {
		t.Fatalf("link u = %q, want a wss:// URL of at most 256 characters", u)
	}
	if len(c) != 32 {
		t.Fatalf("link c is %d bytes, want 32", len(c))
	}
	return relayInvite{u: u, c: c, n: n}
}

type client struct{ conn *websocket.Conn }

// join dials the link's URL, checks the HELLO advertises the link's room, and completes the handshake.
func join(t *testing.T, dialer *websocket.Dialer, u string, wantTag []byte) *client {
	t.Helper()
	conn, resp, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial link URL: %v (%v)", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	c := &client{conn: conn}
	hello := c.read(t)
	if hello["t"] != "hello" || hello["v"] != uint64(1) {
		t.Fatalf("server hello: %v", hello)
	}
	rooms, _ := hello["rooms"].([]any)
	if len(rooms) != 1 {
		t.Fatalf("hello rooms = %v, want the one commons room", hello["rooms"])
	}
	room, _ := rooms[0].(map[string]any)
	if !bytes.Equal(asBytes(room["tag"]), wantTag) {
		t.Fatalf("hello room tag %x does not match the link's room %x", room["tag"], wantTag)
	}
	if room["name"] != e2eName {
		t.Fatalf("hello room name = %v, want %q for a token holder", room["name"], e2eName)
	}
	c.write(t, map[string]any{"t": "hello", "v": 1})
	return c
}

func (c *client) write(t *testing.T, r map[string]any) {
	t.Helper()
	data, err := cbor.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatal(err)
	}
}

func (c *client) read(t *testing.T) map[string]any {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data, err := c.conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatalf("server sent WebSocket message type %d", kind)
	}
	var r map[string]any
	if err := decMode.Unmarshal(data, &r); err != nil {
		t.Fatalf("server record is not a CBOR map: %v", err)
	}
	return r
}

func (c *client) readUntil(t *testing.T, match func(map[string]any) bool) map[string]any {
	t.Helper()
	for i := 0; i < 16; i++ {
		if r := c.read(t); match(r) {
			return r
		}
	}
	t.Fatal("expected record did not arrive within 16 records")
	return nil
}

func asBytes(v any) []byte {
	b, _ := v.([]byte)
	return b
}

// buildSpool returns KNIT_SPOOL_BINARY when set (scripts/remote-arm-test.sh runs this test on a host
// without Go, against a cross-compiled daemon), and otherwise builds the daemon from this checkout.
func buildSpool(t *testing.T) string {
	t.Helper()
	if binary := os.Getenv("KNIT_SPOOL_BINARY"); binary != "" {
		abs, err := filepath.Abs(binary)
		if err != nil {
			t.Fatal(err)
		}
		return abs
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	binary := filepath.Join(t.TempDir(), "knit-spool")
	cmd := exec.Command(goTool, "build", "-trimpath", "-o", binary, "./cmd/knit-spool")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build knit-spool: %v\n%s", err, out)
	}
	return binary
}

// spoolEnv keeps the child's configuration to what the test sets, plus what the OS needs.
func spoolEnv(extra []string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	return append(env, extra...)
}

func runCommand(t *testing.T, binary string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = spoolEnv(env)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("knit-spool %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}

func field(t *testing.T, out, prefix string) string {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), prefix); ok {
			return value
		}
	}
	t.Fatalf("output has no %q line:\n%s", prefix, out)
	return ""
}

func startSpool(t *testing.T, binary string, env []string, roots *x509.CertPool, addr string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, binary, "run")
	cmd.Env = spoolEnv(env)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		if err := <-exited; err != nil && ctx.Err() == nil {
			t.Errorf("knit-spool run: %v", err)
		}
		if t.Failed() {
			t.Logf("knit-spool run output:\n%s", logs.String())
		}
	})

	httpClient := &http.Client{Timeout: time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			t.Fatalf("knit-spool run exited early: %v\n%s", err, logs.String())
		default:
		}
		if resp, err := httpClient.Get("https://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("knit-spool run did not become healthy at %s\n%s", addr, logs.String())
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func selfSignedCert(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "knit-spool e2e"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	roots.AddCert(cert)
	return certFile, keyFile, roots
}
