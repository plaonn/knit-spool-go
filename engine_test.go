package spool

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c := DefaultConfig()
	c.DataPath = ":memory:"
	c.PowBits = 0
	c.RateRecords = 1000
	c.RatePushes = 1000
	c.RateNewScopes = 1000
	return c
}

func newTestEngine(t *testing.T, c Config) *Engine {
	t.Helper()
	e, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func testSub(e *Engine, p *Peer, id []byte, q int64, frames int, ttl int64, maxBlob int) {
	e.Handle(p, record{"t": "sub", "q": q, "subs": []any{map[string]any{
		"scope":  append([]byte(nil), id...),
		"bounds": map[string]any{"maxFrames": int64(frames), "ttlMs": ttl, "maxBlob": int64(maxBlob)},
	}}})
}

func testPush(e *Engine, p *Peer, id, data []byte, q int64) {
	blob := sha256.Sum256(data)
	e.Handle(p, record{"t": "push", "q": q, "scope": append([]byte(nil), id...), "blobId": blob[:], "data": append([]byte(nil), data...)})
}

func drainPeer(p *Peer) []record {
	var got []record
	for len(p.out) > 0 {
		got = append(got, <-p.out)
	}
	return got
}

func TestFramesEvictOldestFanOutAndPersist(t *testing.T) {
	c := testConfig(t)
	path := filepath.Join(t.TempDir(), "spool.db")
	c.DataPath = path
	e, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	base := time.UnixMilli(2_000_000)
	e.now = func() time.Time { return base }
	p1 := e.Register("127.0.0.1")
	p2 := e.Register("127.0.0.2")
	scope := protocolFixture(32, 40)
	testSub(e, p1, scope, 1, 2, 60_000, 64*1024)
	testSub(e, p2, scope, 2, 2, 60_000, 64*1024)
	drainPeer(p1)
	drainPeer(p2)
	first := []byte("frame one")
	testPush(e, p1, scope, first, 3)
	if got := drainPeer(p2); len(got) != 1 || got[0]["t"] != "event" {
		t.Fatalf("subscriber did not receive one event: %#v", got)
	}
	drainPeer(p1)
	base = base.Add(time.Millisecond)
	second := []byte("frame two")
	testPush(e, p1, scope, second, 4)
	drainPeer(p1)
	drainPeer(p2)
	base = base.Add(time.Millisecond)
	third := []byte("frame three")
	testPush(e, p1, scope, third, 5)
	got1 := drainPeer(p1)
	got2 := drainPeer(p2)
	if len(got1) != 2 || got1[0]["t"] != "ok" || got1[1]["t"] != "digest" {
		t.Fatalf("eviction response sequence: %#v", got1)
	}
	if len(got2) != 2 || got2[0]["t"] != "event" || got2[1]["t"] != "digest" {
		t.Fatalf("eviction fanout sequence: %#v", got2)
	}
	e.Handle(p1, record{"t": "list", "q": int64(6), "scope": scope})
	listed := drainPeer(p1)
	if len(listed) != 1 {
		t.Fatalf("list replies: %#v", listed)
	}
	ids, ok := listed[0].array("blobIds")
	if !ok || len(ids) != 2 {
		t.Fatalf("expected two retained frames, got %#v", listed[0])
	}
	tombs, ok := listed[0].array("tombstones")
	firstID := BlobID(first)
	if !ok || len(tombs) != 1 || !equalBytes(tombs[0].([]byte), firstID[:]) {
		t.Fatalf("expected oldest frame tombstone, got %#v", listed[0])
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("database permissions: %v %v", st, err)
	}
	e.Unregister(p1)
	e.Unregister(p2)
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	// Persistence readback is from a fresh engine/store connection.
	e2, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	e2.now = func() time.Time { return base }
	p3 := e2.Register("127.0.0.3")
	testSub(e2, p3, scope, 7, 2, 60_000, 64*1024)
	reply := drainPeer(p3)
	if len(reply) != 1 {
		t.Fatalf("reopened store subscription: %#v", reply)
	}
	digest, ok := reply[0].bytes("digest")
	secondID, thirdID := BlobID(second), BlobID(third)
	if !ok || !equalBytes(digest, digestWire(ScopeDigest([][]byte{secondID[:], thirdID[:]}))) {
		t.Fatalf("persisted digest mismatch: %#v", reply[0])
	}
	e2.Unregister(p3)
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredBoundsClampProtocolIntegerMaximums(t *testing.T) {
	c := testConfig(t)
	e := newTestEngine(t, c)
	p := e.Register("127.0.0.1")
	scope := protocolFixture(32, 41)
	e.Handle(p, record{"t": "sub", "q": int64(1), "subs": []any{map[string]any{
		"scope":  scope,
		"bounds": map[string]any{"maxFrames": int64(1<<31 - 1), "ttlMs": int64(1<<63 - 1), "maxBlob": int64(1<<31 - 1)},
	}}})
	got := drainPeer(p)
	if len(got) != 1 || got[0]["t"] != "digest" {
		t.Fatalf("maximum signed protocol bounds were not clamped: %#v", got)
	}
	state, exists, err := e.getScope(scope)
	if err != nil || !exists {
		t.Fatalf("clamped scope missing: exists=%v err=%v", exists, err)
	}
	if state.MaxFrames != c.MaxFramesCap || state.TTLMS != c.MaxTTLMS || state.MaxBlob != c.MaxBlob {
		t.Fatalf("applied bounds=%#v; caps should be frames=%d ttl=%d blob=%d", state, c.MaxFramesCap, c.MaxTTLMS, c.MaxBlob)
	}
}

func TestAttachmentDeclaredTotalOverQuotaUsesQuotaError(t *testing.T) {
	c := testConfig(t)
	e := newTestEngine(t, c)
	p := e.Register("127.0.0.1")
	scope := protocolFixture(32, 42)
	testSub(e, p, scope, 1, 4, 60_000, 1024)
	drainPeer(p)
	data := []byte("chunk")
	cid := sha256.Sum256(data)
	e.Handle(p, record{"t": "aput", "q": int64(2), "scope": scope, "aid": protocolFixture(32, 43), "idx": int64(0), "total": int64(1<<31 - 1), "cid": cid[:], "data": data})
	got := drainPeer(p)
	if len(got) != 1 || got[0]["t"] != "err" || got[0]["code"] != "quota" {
		t.Fatalf("impossible attachment declaration should use quota error: %#v", got)
	}
}

func TestPushRateLimitReportsRetryAndClosesAfterRepeatedStrikes(t *testing.T) {
	c := testConfig(t)
	c.RateRecords = 100
	c.RatePushes = 1
	e := newTestEngine(t, c)
	now := time.UnixMilli(12_000_000)
	e.now = func() time.Time { return now }
	p := e.Register("127.0.0.1")
	scope := protocolFixture(32, 44)
	testSub(e, p, scope, 1, 16, 60_000, 1024)
	drainPeer(p)
	for i := 0; i < 7; i++ {
		data := []byte{byte(i + 1)}
		testPush(e, p, scope, data, int64(i+2))
	}
	got := drainPeer(p)
	rateErrors := 0
	for _, response := range got {
		if response["t"] == "err" && response["code"] == "rate" {
			rateErrors++
			if retry, ok := response.integer("retryMs"); !ok || retry <= 0 {
				t.Fatalf("rate refusal lacks positive retryMs: %#v", response)
			}
		}
	}
	if rateErrors != 3 {
		t.Fatalf("expected three rate refusals, got %d in %#v", rateErrors, got)
	}
	select {
	case code := <-p.closeCode:
		if code != 4003 {
			t.Fatalf("repeated rate-limit close code=%d, want 4003", code)
		}
	default:
		t.Fatal("three consecutive rate refusals did not close the peer")
	}
}

func TestExpiryAndStorageWatermarkKeepConnectionSubscriptions(t *testing.T) {
	c := testConfig(t)
	c.MaxBytes = 10
	e := newTestEngine(t, c)
	base := time.UnixMilli(10_000_000)
	e.now = func() time.Time { return base }
	p := e.Register("127.0.0.1")
	scope1 := protocolFixture(32, 51)
	testSub(e, p, scope1, 1, 2, 1_000, 1024)
	drainPeer(p)
	testPush(e, p, scope1, []byte("123456"), 2)
	drainPeer(p)
	base = base.Add(2 * time.Second)
	e.Handle(p, record{"t": "list", "q": int64(3), "scope": scope1})
	expired := drainPeer(p)
	if len(expired) != 2 || expired[0]["t"] != "digest" {
		t.Fatalf("expiry list: %#v", expired)
	}
	ids, _ := expired[1].array("blobIds")
	tombs, _ := expired[1].array("tombstones")
	if len(ids) != 0 || len(tombs) != 1 {
		t.Fatalf("expired blob was not tombstoned: %#v", expired[1])
	}
	scope2 := protocolFixture(32, 52)
	testSub(e, p, scope2, 4, 2, 60_000, 1024)
	drainPeer(p)
	base = base.Add(time.Second)
	testPush(e, p, scope2, []byte("abcdef"), 5)
	watermarkReplies := drainPeer(p)
	if len(watermarkReplies) < 1 || watermarkReplies[len(watermarkReplies)-1]["t"] != "ok" {
		t.Fatalf("watermark push response: %#v", watermarkReplies)
	}
	scope3 := protocolFixture(32, 53)
	testSub(e, p, scope3, 6, 2, 60_000, 1024)
	drainPeer(p)
	base = base.Add(time.Second)
	testPush(e, p, scope3, []byte("ghijkl"), 7)
	watermarkReplies = drainPeer(p)
	okFound, anchorFound := false, false
	for _, reply := range watermarkReplies {
		if reply["t"] == "ok" && reply["q"] == int64(7) {
			okFound = true
		}
		if reply["t"] == "digest" && equalBytes(reply["scope"].([]byte), scope2) && reply["count"] == 0 {
			anchorFound = true
		}
	}
	if !okFound || !anchorFound {
		t.Fatalf("watermark shed sequence: %#v", watermarkReplies)
	}
	if _, exists, err := e.getScope(scope1); err != nil || exists {
		t.Fatalf("forgotten scope was not shed: exists=%v err=%v", exists, err)
	}
	if _, exists, err := e.getScope(scope2); err != nil || exists {
		t.Fatalf("least-recent scope was not shed: exists=%v err=%v", exists, err)
	}
	if _, exists, err := e.getScope(scope3); err != nil || !exists {
		t.Fatalf("newest scope was shed: exists=%v err=%v", exists, err)
	}
	// The connection's subscription survives a forgotten scope and list is empty.
	e.Handle(p, record{"t": "list", "q": int64(6), "scope": scope1})
	forgotten := drainPeer(p)
	if len(forgotten) != 1 {
		t.Fatalf("forgotten scope list: %#v", forgotten)
	}
	ids, _ = forgotten[0].array("blobIds")
	if len(ids) != 0 {
		t.Fatalf("forgotten scope contains frames: %#v", forgotten[0])
	}
}

func TestStorageWatermarkUsesRecentScopeActivity(t *testing.T) {
	c := testConfig(t)
	c.MaxBytes = 20
	e := newTestEngine(t, c)
	base := time.UnixMilli(11_000_000)
	e.now = func() time.Time { return base }
	p := e.Register("127.0.0.1")
	scopes := [][]byte{protocolFixture(32, 71), protocolFixture(32, 72), protocolFixture(32, 73), protocolFixture(32, 74)}
	for i := 0; i < 3; i++ {
		testSub(e, p, scopes[i], int64(i+1), 4, 60_000, 1024)
		drainPeer(p)
		base = base.Add(time.Second)
		testPush(e, p, scopes[i], []byte("123456"), int64(i+4))
		drainPeer(p)
	}
	// A valid read makes scope 1 the most recently active. Scope 2 should be shed
	// when scope 4 crosses the cap and the store returns to its 90% low watermark.
	base = base.Add(time.Second)
	e.Handle(p, record{"t": "list", "q": int64(8), "scope": scopes[0]})
	drainPeer(p)
	base = base.Add(time.Second)
	testSub(e, p, scopes[3], 9, 4, 60_000, 1024)
	drainPeer(p)
	base = base.Add(time.Second)
	testPush(e, p, scopes[3], []byte("ghijkl"), 10)
	drainPeer(p)
	for i, want := range []bool{true, false, true, true} {
		_, exists, err := e.getScope(scopes[i])
		if err != nil || exists != want {
			t.Fatalf("scope %d existence=%v want=%v err=%v", i, exists, want, err)
		}
	}
}

func TestCommonsRotationAndDisableReleasePinnedScope(t *testing.T) {
	c := testConfig(t)
	c.DataPath = filepath.Join(t.TempDir(), "commons.db")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 11
	}
	firstID := CommonsScopeID(secret)
	c.Commons = &CommonsConfig{ScopeID: firstID, Name: "one", MaxFrames: 4, TTLMS: 60_000, MaxBlob: 1024, PushRate: 10}
	e1, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	if state, exists, err := e1.getScope(firstID[:]); err != nil || !exists || !state.Commons {
		t.Fatalf("initial commons: %#v exists=%v err=%v", state, exists, err)
	}
	if err := e1.Close(); err != nil {
		t.Fatal(err)
	}
	secret[0]++
	secondID := CommonsScopeID(secret)
	c.Commons = &CommonsConfig{ScopeID: secondID, Name: "two", MaxFrames: 4, TTLMS: 60_000, MaxBlob: 1024, PushRate: 10}
	e2, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	if state, exists, err := e2.getScope(firstID[:]); err != nil || !exists || state.Commons {
		t.Fatalf("rotated commons was not demoted: %#v exists=%v err=%v", state, exists, err)
	}
	if state, exists, err := e2.getScope(secondID[:]); err != nil || !exists || !state.Commons {
		t.Fatalf("replacement commons was not pinned: %#v exists=%v err=%v", state, exists, err)
	}
	if err := e2.Close(); err != nil {
		t.Fatal(err)
	}
	c.Commons = nil
	e3, err := NewEngine(c)
	if err != nil {
		t.Fatal(err)
	}
	if state, exists, err := e3.getScope(secondID[:]); err != nil || !exists || state.Commons {
		t.Fatalf("disabled commons was not demoted: %#v exists=%v err=%v", state, exists, err)
	}
	if err := e3.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentChunkRoundTripAndConflict(t *testing.T) {
	c := testConfig(t)
	c.MaxAttachBytes = 1024
	c.MaxBytes = 1 << 20
	e := newTestEngine(t, c)
	p := e.Register("127.0.0.1")
	scope := protocolFixture(32, 60)
	aid := protocolFixture(32, 61)
	data := []byte("sealed attachment chunk")
	testSub(e, p, scope, 1, 8, 60_000, 1024)
	drainPeer(p)
	cid := sha256.Sum256(data)
	e.Handle(p, record{"t": "aput", "q": int64(2), "scope": scope, "aid": aid, "idx": int64(0), "total": int64(1), "cid": cid[:], "data": data})
	if got := drainPeer(p); len(got) != 1 || got[0]["t"] != "ok" {
		t.Fatalf("aput: %#v", got)
	}
	e.Handle(p, record{"t": "ahave", "q": int64(3), "scope": scope, "aid": aid})
	got := drainPeer(p)
	if len(got) != 1 || got[0]["t"] != "ahas" || got[0]["total"] != 1 || !equalBytes(got[0]["bits"].([]byte), []byte{0x80}) {
		t.Fatalf("ahas bitmap: %#v", got)
	}
	e.Handle(p, record{"t": "aget", "q": int64(4), "scope": scope, "aid": aid, "from": int64(0), "n": int64(64)})
	got = drainPeer(p)
	if len(got) != 2 || got[0]["t"] != "achunk" || got[1]["t"] != "ok" || !equalBytes(got[0]["data"].([]byte), data) {
		t.Fatalf("aget: %#v", got)
	}
	other := []byte("different sealed chunk")
	otherID := sha256.Sum256(other)
	e.Handle(p, record{"t": "aput", "q": int64(5), "scope": scope, "aid": aid, "idx": int64(0), "total": int64(1), "cid": otherID[:], "data": other})
	got = drainPeer(p)
	if len(got) != 1 || got[0]["t"] != "err" || got[0]["code"] != "conflict" {
		t.Fatalf("first-write conflict: %#v", got)
	}
}

func TestPowGateAndCommonsArePinned(t *testing.T) {
	c := testConfig(t)
	c.PowBits = 8
	c.RateNewScopes = 100
	e := newTestEngine(t, c)
	e.now = func() time.Time { return time.UnixMilli(20_680 * 86_400_000) }
	p := e.Register("127.0.0.1")
	scope := protocolFixture(32, 9)
	testSub(e, p, scope, 1, 4, 60_000, 1024)
	got := drainPeer(p)
	if len(got) != 1 || got[0]["code"] != "pow" {
		t.Fatalf("missing PoW should be refused: %#v", got)
	}
	e.Handle(p, record{"t": "sub", "q": int64(2), "subs": []any{map[string]any{
		"scope": scope, "bounds": map[string]any{"maxFrames": int64(4), "ttlMs": int64(60_000), "maxBlob": int64(1024)},
		"pow": map[string]any{"n": int64(8), "d": int64(20_680)},
	}}})
	got = drainPeer(p)
	if len(got) != 1 || got[0]["t"] != "digest" {
		t.Fatalf("valid PoW was not accepted: %#v", got)
	}

	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = 10
	}
	commonsID := CommonsScopeID(secret)
	cc := &CommonsConfig{ScopeID: commonsID, Name: "Home", MaxFrames: 2, TTLMS: 60_000, MaxBlob: 1024, PushRate: 100}
	c.Commons = cc
	e2 := newTestEngine(t, c)
	hello := e2.Hello()
	common, ok := hello.nested("commons")
	if !ok || common["name"] != "Home" || common["maxFrames"] != 2 {
		t.Fatalf("commons hello fields: %#v", hello)
	}
	if _, exposesScope := common["scope"]; exposesScope {
		t.Fatal("commons scope id must never appear in HELLO")
	}
	p2 := e2.Register("127.0.0.2")
	testSub(e2, p2, commonsID[:], 1, 1, 1, 1)
	got = drainPeer(p2)
	if len(got) != 1 || got[0]["t"] != "digest" {
		t.Fatalf("commons subscribe: %#v", got)
	}
	boundary, ok := got[0].nested("bounds")
	if !ok || boundary["maxFrames"] != 2 || boundary["ttlMs"] != int64(60_000) || boundary["maxBlob"] != 1024 {
		t.Fatalf("commons bounds were not pinned: %#v", got[0])
	}
}
