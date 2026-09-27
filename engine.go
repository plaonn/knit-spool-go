package spool

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type bounds struct {
	MaxFrames int
	TTLMS     int64
	MaxBlob   int
}

type bucket struct {
	rate   int
	period time.Duration
	tokens float64
	last   time.Time
}

func newBucket(rate int, now time.Time) bucket {
	if rate <= 0 {
		return bucket{}
	}
	return bucket{rate: rate, period: time.Second, tokens: float64(rate * 4), last: now}
}

func newWindowBucket(rate int, period time.Duration, now time.Time) bucket {
	if rate <= 0 {
		return bucket{}
	}
	return bucket{rate: rate, period: period, tokens: float64(rate * 4), last: now}
}

func (b *bucket) allow(now time.Time, cost float64) (bool, int64) {
	if b.rate <= 0 {
		return true, 0
	}
	if b.last.IsZero() {
		b.last = now
		b.tokens = float64(b.rate * 4)
	}
	if now.After(b.last) {
		b.tokens += now.Sub(b.last).Seconds() / b.period.Seconds() * float64(b.rate)
		burst := float64(b.rate * 4)
		if b.tokens > burst {
			b.tokens = burst
		}
		b.last = now
	}
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	missing := cost - b.tokens
	return false, int64(missing/float64(b.rate)*1000 + 1)
}

type Peer struct {
	remote    string
	out       chan record
	closed    chan struct{}
	closeCode chan int
	closeOnce sync.Once
	subs      map[string]bounds
	records   bucket
	pushes    bucket
	strikes   int
	strikeAt  time.Time
	joinedAt  time.Time
}

func newPeer(remote string, cfg Config, now time.Time) *Peer {
	return &Peer{
		remote: remote, out: make(chan record, 128), closed: make(chan struct{}),
		closeCode: make(chan int, 1), subs: make(map[string]bounds),
		records: newBucket(cfg.RateRecords, now), pushes: newBucket(cfg.RatePushes, now), joinedAt: now,
	}
}

func (p *Peer) send(r record) {
	select {
	case <-p.closed:
		return
	case p.out <- r:
	default:
		p.close(1013)
	}
}

func (p *Peer) close(code int) {
	p.closeOnce.Do(func() {
		select {
		case p.closeCode <- code:
		default:
		}
		close(p.closed)
	})
}

type Counters struct {
	Connections      atomic.Int64
	ConnectionsTotal atomic.Int64
	Records          atomic.Int64
	Pushes           atomic.Int64
	Events           atomic.Int64
	PowVerified      atomic.Int64
	RateLimited      atomic.Int64
	ScopesShed       atomic.Int64
	ChunksStored     atomic.Int64
	EgressBytes      atomic.Int64
}

type Engine struct {
	cfg           Config
	store         *Store
	mu            sync.Mutex
	peers         map[*Peer]struct{}
	newScopes     map[string]*bucket
	commonsPush   bucket
	lastGateError protocolError
	lastRetry     int64
	counters      Counters
	now           func() time.Time
}

func NewEngine(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	store, err := OpenStore(cfg.DataPath)
	if err != nil {
		return nil, err
	}
	now := time.Now
	e := &Engine{
		cfg: cfg, store: store, peers: make(map[*Peer]struct{}),
		newScopes: make(map[string]*bucket), commonsPush: newBucket(0, now()), now: now,
	}
	if cfg.Commons != nil {
		e.commonsPush = newBucket(cfg.Commons.PushRate, now())
	}
	if err := e.ensureCommons(); err != nil {
		store.Close()
		return nil, err
	}
	return e, nil
}

func (e *Engine) Close() error { return e.store.Close() }

func (e *Engine) Register(remote string) *Peer {
	now := e.now()
	p := newPeer(remote, e.cfg, now)
	e.mu.Lock()
	e.peers[p] = struct{}{}
	e.counters.Connections.Add(1)
	e.counters.ConnectionsTotal.Add(1)
	e.mu.Unlock()
	return p
}

func (e *Engine) Unregister(p *Peer) {
	e.mu.Lock()
	if _, ok := e.peers[p]; ok {
		delete(e.peers, p)
		e.counters.Connections.Add(-1)
	}
	e.mu.Unlock()
	p.close(1000)
}

func (e *Engine) Hello() record {
	limits := record{
		"maxBlob": e.cfg.MaxBlob, "maxRecord": e.cfg.MaxRecord, "maxScopes": e.cfg.MaxScopes,
		"maxPull": e.cfg.MaxPull, "maxFramesCap": e.cfg.MaxFramesCap, "maxTtlMs": e.cfg.MaxTTLMS,
	}
	if e.cfg.AttachmentsEnabled() {
		limits["maxAttachBytes"] = e.cfg.MaxAttachBytes
		limits["maxAChunk"] = e.cfg.MaxAChunk
		limits["maxAget"] = e.cfg.MaxAget
	}
	hello := record{"t": "hello", "v": ProtocolVersion, "min": ProtocolVersion, "limits": limits, "powBits": e.cfg.PowBits}
	if e.cfg.RequireModeration {
		hello["moderation"] = true
	}
	if cc := e.cfg.Commons; cc != nil {
		commons := record{"maxFrames": cc.MaxFrames, "ttlMs": cc.TTLMS, "maxBlob": cc.MaxBlob}
		if cc.Name != "" {
			commons["name"] = cc.Name
		}
		if cc.Attach {
			commons["attach"] = true
		}
		hello["commons"] = commons
	}
	return hello
}

func (e *Engine) ensureCommons() error {
	cc := e.cfg.Commons
	now := e.now().UnixMilli()
	err := e.tx(func(tx *sql.Tx) error {
		if cc == nil {
			_, err := tx.Exec("UPDATE scopes SET is_commons=0 WHERE is_commons=1")
			return err
		}
		// A rotated or disabled commons becomes an ordinary retained scope. It must no longer be
		// pinned against the storage watermark.
		if _, err := tx.Exec("UPDATE scopes SET is_commons=0 WHERE is_commons=1 AND scope<>?", cc.ScopeID[:]); err != nil {
			return err
		}
		var isCommons int
		err := tx.QueryRow("SELECT is_commons FROM scopes WHERE scope=?", cc.ScopeID[:]).Scan(&isCommons)
		if err == nil {
			if isCommons != 1 {
				return errors.New("commons scope id collides with an existing scope")
			}
			_, err = tx.Exec("UPDATE scopes SET max_frames=?,ttl_ms=?,max_blob=? WHERE scope=?", cc.MaxFrames, cc.TTLMS, cc.MaxBlob, cc.ScopeID[:])
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM scopes").Scan(&count); err != nil {
			return err
		}
		if count >= e.cfg.MaxScopes {
			return errors.New("commons cannot fit under SPOOL_MAX_SCOPES")
		}
		_, err = tx.Exec("INSERT INTO scopes(scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active) VALUES(?,?,?,?,1,?,?)",
			cc.ScopeID[:], cc.MaxFrames, cc.TTLMS, cc.MaxBlob, now, now)
		return err
	})
	if err != nil {
		return err
	}
	if cc == nil {
		return nil
	}
	return e.applyFrameBounds(cc.ScopeID[:], bounds{MaxFrames: cc.MaxFrames, TTLMS: cc.TTLMS, MaxBlob: cc.MaxBlob}, now)
}

func (e *Engine) tx(fn func(*sql.Tx) error) error {
	tx, err := e.store.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (e *Engine) getScope(id []byte) (scopeState, bool, error) {
	var s scopeState
	var commons int
	err := e.store.db.QueryRow("SELECT scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active FROM scopes WHERE scope=?", id).
		Scan(&s.ID, &s.MaxFrames, &s.TTLMS, &s.MaxBlob, &commons, &s.CreatedAt, &s.LastActive)
	if err == sql.ErrNoRows {
		return scopeState{}, false, nil
	}
	if err != nil {
		return scopeState{}, false, err
	}
	s.Commons = commons != 0
	return s, true, nil
}

func (e *Engine) countFrames(id []byte) (int, error) {
	var count int
	err := e.store.db.QueryRow("SELECT count(*) FROM frames WHERE scope=?", id).Scan(&count)
	return count, err
}

func (e *Engine) frameIDs(id []byte) ([][]byte, error) {
	rows, err := e.store.db.Query("SELECT blob_id FROM frames WHERE scope=? ORDER BY arrived_at,blob_id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids [][]byte
	for rows.Next() {
		var item []byte
		if err := rows.Scan(&item); err != nil {
			return nil, err
		}
		ids = append(ids, item)
	}
	return ids, rows.Err()
}

func (e *Engine) currentDigest(id []byte, s scopeState) (record, error) {
	ids, err := e.frameIDs(id)
	if err != nil {
		return nil, err
	}
	return digestRecord(id, ScopeDigest(ids), len(ids), s.MaxFrames, s.TTLMS, s.MaxBlob), nil
}

func (e *Engine) broadcast(id []byte, r record, except *Peer) {
	for p := range e.peers {
		if p == except {
			continue
		}
		if _, ok := p.subs[string(id)]; ok {
			p.send(r)
		}
	}
}

func (e *Engine) broadcastDigest(id []byte, except *Peer) error {
	s, ok, err := e.getScope(id)
	if err != nil || !ok {
		return err
	}
	d, err := e.currentDigest(id, s)
	if err != nil {
		return err
	}
	e.broadcast(id, d, except)
	return nil
}

func (e *Engine) digestForForgotten(id []byte, b bounds) record {
	return digestRecord(id, 0, 0, b.MaxFrames, b.TTLMS, b.MaxBlob)
}

func (e *Engine) Handle(p *Peer, r record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counters.Records.Add(1)
	t := mustRecordType(r)
	qValue, hasQ := r.integer("q")
	var qPtr *int64
	if hasQ && qValue >= 0 {
		qPtr = &qValue
	}
	now := e.now()
	if t == "sub" {
		if ok, retry := p.records.allow(now, 1); !ok {
			e.refuseRate(p, qPtr, nil, retry, true)
			return
		}
		if qPtr == nil {
			p.send(errorRecord("malformed", nil, nil, 0))
			return
		}
		e.handleSub(p, r, qValue, now)
		return
	}
	if ok, retry := p.records.allow(now, 1); !ok {
		e.refuseRate(p, qPtr, recordScope(r), retry, true)
		return
	}
	if t == "hello" {
		p.send(errorRecord("malformed", nil, nil, 0))
		return
	}
	if _, ok := r.text("t"); !ok || t == "" {
		p.send(errorRecord("malformed", qPtr, recordScope(r), 0))
		return
	}
	if !isKnownRecord(t) {
		return
	}
	if qPtr == nil {
		p.send(errorRecord("malformed", nil, nil, 0))
		return
	}
	q := qValue
	if t == "push" || t == "aput" {
		if ok, retry := p.pushes.allow(now, 1); !ok {
			e.refuseRate(p, &q, recordScope(r), retry, true)
			return
		}
	}
	switch t {
	case "list":
		e.handleList(p, r, q, now)
	case "pull":
		e.handlePull(p, r, q, now)
	case "push":
		e.handlePush(p, r, q, now)
	case "ahave":
		e.handleAHave(p, r, q, now)
	case "aget":
		e.handleAGet(p, r, q, now)
	case "aput":
		e.handleAPut(p, r, q, now)
	}
}

// HandleMalformed charges the record bucket before answering a transport or CBOR error.
func (e *Engine) HandleMalformed(p *Peer, code string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counters.Records.Add(1)
	now := e.now()
	if ok, retry := p.records.allow(now, 1); !ok {
		e.refuseRate(p, nil, nil, retry, true)
		return
	}
	p.send(errorRecord(code, nil, nil, 0))
}

func isKnownRecord(t string) bool {
	switch t {
	case "sub", "list", "pull", "push", "ahave", "aget", "aput":
		return true
	default:
		return false
	}
}

func recordScope(r record) []byte {
	b, _ := r.bytes("scope")
	if len(b) != ScopeIDBytes {
		return nil
	}
	return b
}

func (e *Engine) refuseRate(p *Peer, q *int64, scope []byte, retry int64, strike bool) {
	e.counters.RateLimited.Add(1)
	if strike {
		now := e.now()
		if p.strikeAt.IsZero() || now.Sub(p.strikeAt) > time.Minute {
			p.strikeAt = now
			p.strikes = 0
		}
		p.strikes++
		p.send(errorRecord("rate", q, scope, retry))
		if p.strikes >= 3 {
			p.close(4003)
		}
		return
	}
	p.send(errorRecord("rate", q, scope, retry))
}

func (e *Engine) handleSub(p *Peer, r record, q int64, now time.Time) {
	entries, ok := r.array("subs")
	if !ok || len(entries) > e.cfg.MaxScopes {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	seen := make(map[string]struct{}, len(entries))
	parsed := make([]struct {
		id  []byte
		b   bounds
		pow record
	}, 0, len(entries))
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			p.send(errorRecord("malformed", &q, nil, 0))
			return
		}
		item := record(entry)
		id, err := requireBytes(item, "scope", ScopeIDBytes)
		if err != nil {
			p.send(errorRecord("malformed", &q, nil, 0))
			return
		}
		key := string(id)
		if _, duplicate := seen[key]; duplicate {
			p.send(errorRecord("malformed", &q, nil, 0))
			return
		}
		seen[key] = struct{}{}
		declared, ok := item.nested("bounds")
		if !ok {
			p.send(errorRecord("malformed", &q, id, 0))
			return
		}
		frames, err1 := requireInt(declared, "maxFrames", 1, 1<<31-1)
		ttl, err2 := requireInt(declared, "ttlMs", 1, 1<<63-1)
		blob, err3 := requireInt(declared, "maxBlob", 1, 1<<31-1)
		if err1 != nil || err2 != nil || err3 != nil {
			p.send(errorRecord("malformed", &q, id, 0))
			return
		}
		b := bounds{MaxFrames: min(int(frames), e.cfg.MaxFramesCap), TTLMS: min(ttl, e.cfg.MaxTTLMS), MaxBlob: min(int(blob), e.cfg.MaxBlob)}
		pow, _ := item.nested("pow")
		parsed = append(parsed, struct {
			id  []byte
			b   bounds
			pow record
		}{append([]byte(nil), id...), b, pow})
	}
	strikeCounted := false
	for index, item := range parsed {
		if index > 0 {
			if allowed, retry := p.records.allow(now, 1); !allowed {
				for _, rest := range parsed[index:] {
					e.refuseRate(p, &q, rest.id, retry, !strikeCounted)
					strikeCounted = true
				}
				return
			}
		}
		if err := e.subscribe(p, item.id, item.b, item.pow, q, now); err != nil {
			var cause protocolError
			if errors.As(err, &cause) {
				if cause.code == "rate" {
					e.refuseRate(p, &q, item.id, cause.retryMS, cause.strike && !strikeCounted)
					strikeCounted = true
				} else {
					p.send(errorRecord(cause.code, &q, item.id, cause.retryMS))
				}
			} else {
				p.send(errorRecord("internal", &q, item.id, 0))
			}
		}
	}
}

type protocolError struct {
	code    string
	retryMS int64
	strike  bool
}

func (p protocolError) Error() string { return p.code }

func (e *Engine) subscribe(p *Peer, id []byte, requested bounds, pow record, q int64, now time.Time) error {
	nowMS := now.UnixMilli()
	s, exists, err := e.getScope(id)
	if err != nil {
		return err
	}
	if e.cfg.Commons != nil && string(id) == string(e.cfg.Commons.ScopeID[:]) {
		cc := e.cfg.Commons
		requested = bounds{MaxFrames: cc.MaxFrames, TTLMS: cc.TTLMS, MaxBlob: cc.MaxBlob}
		s, exists, err = e.getScope(id)
		if err != nil {
			return err
		}
	}
	if !exists {
		if e.cfg.Commons != nil && string(id) == string(e.cfg.Commons.ScopeID[:]) {
			return protocolError{code: "internal"}
		}
		if !e.allowNewScope(p, id, pow, now) {
			return e.lastGateError
		}
		var count int
		if err := e.store.db.QueryRow("SELECT count(*) FROM scopes").Scan(&count); err != nil {
			return err
		}
		if count >= e.cfg.MaxScopes {
			return protocolError{code: "quota"}
		}
		if err := e.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO scopes(scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active) VALUES(?,?,?, ?,0,?,?)",
				id, requested.MaxFrames, requested.TTLMS, requested.MaxBlob, nowMS, nowMS)
			return err
		}); err != nil {
			return err
		}
	} else {
		if err := e.expireScope(id, s, nowMS); err != nil {
			return err
		}
		if err := e.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec("UPDATE scopes SET max_frames=?,ttl_ms=?,max_blob=?,last_active=? WHERE scope=? AND is_commons=0",
				requested.MaxFrames, requested.TTLMS, requested.MaxBlob, nowMS, id)
			if s.Commons {
				_, err = tx.Exec("UPDATE scopes SET last_active=? WHERE scope=?", nowMS, id)
			}
			return err
		}); err != nil {
			return err
		}
	}
	p.subs[string(id)] = requested
	if err := e.applyFrameBounds(id, requested, nowMS); err != nil {
		return err
	}
	_ = e.broadcastDigest(id, nil)
	return nil
}

var noGateError = protocolError{code: "pow"}

func (e *Engine) allowNewScope(p *Peer, id []byte, pow record, now time.Time) bool {
	e.lastGateError = protocolError{}
	e.lastRetry = 0
	if !e.allowNewScopeRate(p.remote, now) {
		e.lastGateError = protocolError{code: "rate", retryMS: e.lastRetry, strike: true}
		return false
	}
	if e.cfg.PowBits == 0 {
		return true
	}
	if pow == nil {
		e.lastGateError = noGateError
		return false
	}
	n, ok1 := pow.integer("n")
	d, ok2 := pow.integer("d")
	if !ok1 || !ok2 || n < 0 || d < 0 {
		e.lastGateError = noGateError
		return false
	}
	var cached int
	err := e.store.db.QueryRow("SELECT 1 FROM pow_cache WHERE scope=? AND day=?", id, d).Scan(&cached)
	if err == nil {
		return true
	}
	if err != sql.ErrNoRows {
		e.lastGateError = protocolError{code: "internal"}
		return false
	}
	if !VerifyPow(id, uint64(d), uint64(n), now.UnixMilli(), e.cfg.PowBits) {
		e.lastGateError = noGateError
		return false
	}
	if err := e.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT OR IGNORE INTO pow_cache(scope,day,accepted_at) VALUES(?,?,?)", id, d, now.UnixMilli())
		return err
	}); err != nil {
		e.lastGateError = protocolError{code: "internal"}
		return false
	}
	e.counters.PowVerified.Add(1)
	return true
}

func (e *Engine) allowNewScopeRate(remote string, now time.Time) bool {
	key := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		key = host
	}
	if ip := net.ParseIP(key); ip != nil && ip.To4() == nil {
		key = ip.Mask(net.CIDRMask(64, 128)).String()
	}
	b := e.newScopes[key]
	if b == nil {
		initial := newWindowBucket(e.cfg.RateNewScopes, time.Minute, now)
		b = &initial
		e.newScopes[key] = b
	}
	ok, retry := b.allow(now, 1)
	if !ok {
		e.lastRetry = retry
	}
	return ok
}

func (e *Engine) handleList(p *Peer, r record, q int64, now time.Time) {
	id, err := requireBytes(r, "scope", ScopeIDBytes)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	b, subscribed := p.subs[string(id)]
	if !subscribed {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	s, exists, err := e.getScope(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if !exists {
		p.send(record{"t": "list", "q": q, "scope": id, "blobIds": []any{}, "tombstones": []any{}})
		return
	}
	if err := e.expireScope(id, s, now.UnixMilli()); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if err := e.touchScope(id, now.UnixMilli()); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	ids, err := e.frameIDs(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	tombs, err := e.tombstoneIDs(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if b.MaxFrames == 0 {
		b = bounds{s.MaxFrames, s.TTLMS, s.MaxBlob}
	}
	p.send(record{"t": "list", "q": q, "scope": id, "blobIds": byteSlices(ids), "tombstones": byteSlices(tombs)})
}

func (e *Engine) tombstoneIDs(id []byte) ([][]byte, error) {
	rows, err := e.store.db.Query("SELECT blob_id FROM tombstones WHERE scope=? ORDER BY sequence", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids [][]byte
	for rows.Next() {
		var v []byte
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		ids = append(ids, v)
	}
	return ids, rows.Err()
}

func (e *Engine) handlePull(p *Peer, r record, q int64, now time.Time) {
	id, err := requireBytes(r, "scope", ScopeIDBytes)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	if _, subscribed := p.subs[string(id)]; !subscribed {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	requested, ok := r.array("blobIds")
	if !ok {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	selected := requested
	if len(selected) > e.cfg.MaxPull {
		selected = selected[:e.cfg.MaxPull]
	}
	for _, raw := range selected {
		v, ok := raw.([]byte)
		if !ok || len(v) != 32 {
			p.send(errorRecord("malformed", &q, id, 0))
			return
		}
	}
	s, exists, err := e.getScope(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if exists {
		if err := e.expireScope(id, s, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if err := e.touchScope(id, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	missing := make([][]byte, 0)
	for _, raw := range selected {
		blobID := raw.([]byte)
		var data []byte
		err := e.store.db.QueryRow("SELECT data FROM frames WHERE scope=? AND blob_id=?", id, blobID).Scan(&data)
		if err == sql.ErrNoRows {
			missing = append(missing, append([]byte(nil), blobID...))
			continue
		}
		if err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		p.send(record{"t": "blob", "scope": id, "blobId": blobID, "data": data})
	}
	p.send(okRecord(q, missingIfAny(missing)))
}

func missingIfAny(values [][]byte) [][]byte {
	if len(values) == 0 {
		return nil
	}
	return values
}

func (e *Engine) handlePush(p *Peer, r record, q int64, now time.Time) {
	id, err := requireBytes(r, "scope", ScopeIDBytes)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	if e.cfg.Commons != nil && string(id) == string(e.cfg.Commons.ScopeID[:]) {
		if allowed, retry := e.commonsPush.allow(now, 1); !allowed {
			e.refuseRate(p, &q, id, retry, false)
			return
		}
	}
	b, subscribed := p.subs[string(id)]
	if !subscribed {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	blobID, err := requireBytes(r, "blobId", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	data, err := requireBytes(r, "data", -1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if len(data) > b.MaxBlob {
		p.send(errorRecord("too_large", &q, id, 0))
		return
	}
	actual := sha256.Sum256(data)
	if !equalBytes(actual[:], blobID) {
		p.send(errorRecord("bad_id", &q, id, 0))
		return
	}
	nowMS := now.UnixMilli()
	s, exists, err := e.getScope(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	recreated := !exists
	if exists {
		if err := e.expireScope(id, s, nowMS); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		var tomb int
		err := e.store.db.QueryRow("SELECT 1 FROM tombstones WHERE scope=? AND blob_id=?", id, blobID).Scan(&tomb)
		if err == nil {
			p.send(errorRecord("tombstoned", &q, id, 0))
			return
		}
		if err != sql.ErrNoRows {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		var existsFrame int
		err = e.store.db.QueryRow("SELECT 1 FROM frames WHERE scope=? AND blob_id=?", id, blobID).Scan(&existsFrame)
		if err == nil {
			p.send(okRecord(q, nil))
			return
		}
		if err != sql.ErrNoRows {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	} else {
		if len(data) > 0 && e.cfg.MaxBytes > 0 {
			pinned, err := e.pinnedBytes()
			if err != nil {
				p.send(errorRecord("internal", &q, id, 0))
				return
			}
			if pinned+int64(len(data)) > e.cfg.MaxBytes {
				p.send(errorRecord("quota", &q, id, 0))
				return
			}
		}
		if !e.allowNewScope(p, id, r.nestedOrNil("pow"), now) {
			gate := e.lastGateError
			if gate.code == "rate" {
				e.refuseRate(p, &q, id, gate.retryMS, gate.strike)
			} else {
				p.send(errorRecord(gate.code, &q, id, 0))
			}
			return
		}
		var count int
		if err := e.store.db.QueryRow("SELECT count(*) FROM scopes").Scan(&count); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if count >= e.cfg.MaxScopes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		if err := e.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO scopes(scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active) VALUES(?,?,?, ?,0,?,?)",
				id, b.MaxFrames, b.TTLMS, b.MaxBlob, nowMS, nowMS)
			return err
		}); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	if recreated {
		p.subs[string(id)] = b
		p.send(e.digestForForgotten(id, b))
	}
	s, exists, err = e.getScope(id)
	if err != nil || !exists {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	var tomb int
	err = e.store.db.QueryRow("SELECT 1 FROM tombstones WHERE scope=? AND blob_id=?", id, blobID).Scan(&tomb)
	if err == nil {
		p.send(errorRecord("tombstoned", &q, id, 0))
		return
	}
	if err != sql.ErrNoRows {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if e.cfg.MaxBytes > 0 {
		pinned, err := e.pinnedBytes()
		if err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if pinned+int64(len(data)) > e.cfg.MaxBytes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
	}
	evicted, err := e.insertFrame(id, blobID, data, s, nowMS)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	e.counters.Pushes.Add(1)
	// The Kotlin reference acknowledges the accepted push before sending fan-out
	// records. Keep q-correlated responses ahead of unsolicited digest re-anchors.
	p.send(okRecord(q, nil))
	e.broadcast(id, record{"t": "event", "scope": id, "blobId": blobID, "data": data}, p)
	e.counters.Events.Add(int64(e.subscriberCount(id, p)))
	if recreated || evicted {
		_ = e.broadcastDigest(id, nil)
	}
	// The storage watermark runs after the write and its protocol response, as it
	// does in the reference daemon. A storage-side failure is retried by Sweep.
	if err := e.enforceWatermark(); err != nil {
		e.lastGateError = protocolError{code: "internal"}
	}
}

func (r record) nestedOrNil(key string) record { v, _ := r.nested(key); return v }

func (e *Engine) subscriberCount(id []byte, except *Peer) int {
	n := 0
	for p := range e.peers {
		if p != except {
			if _, ok := p.subs[string(id)]; ok {
				n++
			}
		}
	}
	return n
}

func (e *Engine) insertFrame(id, blobID, data []byte, s scopeState, now int64) (bool, error) {
	evicted := false
	err := e.tx(func(tx *sql.Tx) error {
		var stillExists int
		err := tx.QueryRow("SELECT 1 FROM frames WHERE scope=? AND blob_id=?", id, blobID).Scan(&stillExists)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.Exec("INSERT INTO frames(scope,blob_id,data,arrived_at) VALUES(?,?,?,?)", id, blobID, data, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE scopes SET last_active=? WHERE scope=?", now, id)
		if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM frames WHERE scope=?", id).Scan(&count); err != nil {
			return err
		}
		for count > s.MaxFrames {
			var oldID []byte
			var arrived int64
			if err := tx.QueryRow("SELECT blob_id,arrived_at FROM frames WHERE scope=? ORDER BY arrived_at,blob_id LIMIT 1", id).Scan(&oldID, &arrived); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM frames WHERE scope=? AND blob_id=?", id, oldID); err != nil {
				return err
			}
			if err := e.addTombstoneTx(tx, id, oldID, now+s.TTLMS, s.MaxFrames); err != nil {
				return err
			}
			evicted = true
			count--
		}
		return nil
	})
	return evicted, err
}

func (e *Engine) addTombstoneTx(tx *sql.Tx, scope, id []byte, expires int64, maxFrames int) error {
	var sequence int64
	if err := tx.QueryRow("SELECT next_value FROM sequence WHERE id=1").Scan(&sequence); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE sequence SET next_value=? WHERE id=1", sequence+1); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO tombstones(scope,blob_id,expires_at,sequence) VALUES(?,?,?,?) ON CONFLICT(scope,blob_id) DO UPDATE SET expires_at=excluded.expires_at,sequence=excluded.sequence", scope, id, expires, sequence); err != nil {
		return err
	}
	return e.trimTombstonesTx(tx, scope, maxFrames)
}

func (e *Engine) trimTombstonesTx(tx *sql.Tx, scope []byte, maxFrames int) error {
	cap := max(2*maxFrames, 1024)
	rows, err := tx.Query("SELECT blob_id FROM tombstones WHERE scope=? ORDER BY sequence DESC LIMIT -1 OFFSET ?", scope, cap)
	if err != nil {
		return err
	}
	var remove [][]byte
	for rows.Next() {
		var id []byte
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		remove = append(remove, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range remove {
		if _, err := tx.Exec("DELETE FROM tombstones WHERE scope=? AND blob_id=?", scope, id); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) handleAHave(p *Peer, r record, q int64, now time.Time) {
	if !e.cfg.AttachmentsEnabled() {
		p.send(errorRecord("malformed", &q, recordScope(r), 0))
		return
	}
	id, err := requireBytes(r, "scope", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	aid, err := requireBytes(r, "aid", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if _, ok := p.subs[string(id)]; !ok {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	if e.isCommonsAttachmentDisabled(id) {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if s, exists, err := e.getScope(id); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	} else if exists {
		if err := e.expireScope(id, s, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if err := e.touchScope(id, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	bits, total, dead, err := e.attachmentBits(id, aid, now.UnixMilli())
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	response := record{"t": "ahas", "q": q, "scope": id, "aid": aid, "total": total, "bits": bits}
	if dead {
		response["dead"] = true
	}
	p.send(response)
}

func (e *Engine) isCommonsAttachmentDisabled(id []byte) bool {
	return e.cfg.Commons != nil && string(id) == string(e.cfg.Commons.ScopeID[:]) && !e.cfg.Commons.Attach
}

func (e *Engine) attachmentBits(scope, aid []byte, now int64) ([]byte, int, bool, error) {
	var total int
	err := e.store.db.QueryRow("SELECT total FROM attachments WHERE scope=? AND aid=?", scope, aid).Scan(&total)
	if err == sql.ErrNoRows {
		var dead int
		err = e.store.db.QueryRow("SELECT 1 FROM attachment_tombstones WHERE scope=? AND aid=? AND expires_at>?", scope, aid, now).Scan(&dead)
		if err == nil {
			return []byte{}, 0, true, nil
		}
		if err != sql.ErrNoRows {
			return nil, 0, false, err
		}
		return []byte{}, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	bitmap := make([]byte, (total+7)/8)
	rows, err := e.store.db.Query("SELECT idx FROM chunks WHERE scope=? AND aid=?", scope, aid)
	if err != nil {
		return nil, 0, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, 0, false, err
		}
		if idx >= 0 && idx < total {
			bitmap[idx/8] |= byte(0x80 >> uint(idx%8))
		}
	}
	return bitmap, total, false, rows.Err()
}

func (e *Engine) handleAGet(p *Peer, r record, q int64, now time.Time) {
	if !e.cfg.AttachmentsEnabled() {
		p.send(errorRecord("malformed", &q, recordScope(r), 0))
		return
	}
	id, err := requireBytes(r, "scope", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	aid, err := requireBytes(r, "aid", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	from, err := requireInt(r, "from", 0, 1<<31-1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	n, err := requireInt(r, "n", 0, 1<<31-1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if _, ok := p.subs[string(id)]; !ok {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	if e.isCommonsAttachmentDisabled(id) {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	s, exists, err := e.getScope(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if exists {
		if err := e.expireScope(id, s, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if err := e.touchScope(id, now.UnixMilli()); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	if n > int64(e.cfg.MaxAget) {
		n = int64(e.cfg.MaxAget)
	}
	var total int
	err = e.store.db.QueryRow("SELECT total FROM attachments WHERE scope=? AND aid=?", id, aid).Scan(&total)
	if err == nil {
		end := min(int64(total), from+n)
		for idx := from; idx < end; idx++ {
			var cid, data []byte
			err := e.store.db.QueryRow("SELECT cid,data FROM chunks WHERE scope=? AND aid=? AND idx=?", id, aid, idx).Scan(&cid, &data)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				p.send(errorRecord("internal", &q, id, 0))
				return
			}
			p.send(record{"t": "achunk", "scope": id, "aid": aid, "idx": idx, "total": total, "cid": cid, "data": data})
		}
	} else if err != sql.ErrNoRows {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	p.send(okRecord(q, nil))
}

func (e *Engine) handleAPut(p *Peer, r record, q int64, now time.Time) {
	if !e.cfg.AttachmentsEnabled() {
		p.send(errorRecord("malformed", &q, recordScope(r), 0))
		return
	}
	id, err := requireBytes(r, "scope", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, nil, 0))
		return
	}
	aid, err := requireBytes(r, "aid", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	idx, err := requireInt(r, "idx", 0, 1<<31-1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	total, err := requireInt(r, "total", 1, 1<<31-1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	cid, err := requireBytes(r, "cid", 32)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	data, err := requireBytes(r, "data", -1)
	if err != nil {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if int64(idx) >= total {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if len(data) > e.cfg.MaxAChunk {
		p.send(errorRecord("too_large", &q, id, 0))
		return
	}
	h := sha256.Sum256(data)
	if !equalBytes(h[:], cid) {
		p.send(errorRecord("bad_id", &q, id, 0))
		return
	}
	if _, ok := p.subs[string(id)]; !ok {
		p.send(errorRecord("not_subscribed", &q, id, 0))
		return
	}
	if e.isCommonsAttachmentDisabled(id) {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if e.cfg.Commons != nil && string(id) == string(e.cfg.Commons.ScopeID[:]) {
		if allowed, retry := e.commonsPush.allow(now, 1); !allowed {
			e.refuseRate(p, &q, id, retry, false)
			return
		}
	}
	maxTotal := int64((e.cfg.MaxAttachBytes + 49_151) / 49_152)
	if total > maxTotal {
		p.send(errorRecord("quota", &q, id, 0))
		return
	}
	nowMS := now.UnixMilli()
	s, exists, err := e.getScope(id)
	if err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	if exists {
		if err := e.expireScope(id, s, nowMS); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if err := e.touchScope(id, nowMS); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	if !exists {
		if int64(max(len(data), 512)) > e.cfg.MaxAttachBytes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		pinned, err := e.pinnedBytes()
		if err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if e.cfg.MaxBytes > 0 && pinned+int64(max(len(data), 512)) > e.cfg.MaxBytes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		b := p.subs[string(id)]
		if !e.allowNewScope(p, id, r.nestedOrNil("pow"), now) {
			gate := e.lastGateError
			if gate.code == "rate" {
				e.refuseRate(p, &q, id, gate.retryMS, gate.strike)
			} else {
				p.send(errorRecord(gate.code, &q, id, 0))
			}
			return
		}
		var count int
		if err := e.store.db.QueryRow("SELECT count(*) FROM scopes").Scan(&count); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if count >= e.cfg.MaxScopes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		if err := e.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec("INSERT INTO scopes(scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active) VALUES(?,?,?, ?,0,?,?)", id, b.MaxFrames, b.TTLMS, b.MaxBlob, nowMS, nowMS)
			return err
		}); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		p.send(e.digestForForgotten(id, b))
	}
	var dead int
	err = e.store.db.QueryRow("SELECT 1 FROM attachment_tombstones WHERE scope=? AND aid=?", id, aid).Scan(&dead)
	if err == nil {
		p.send(errorRecord("tombstoned", &q, id, 0))
		return
	}
	if err != sql.ErrNoRows {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	charge := int64(max(len(data), 512))
	if charge > e.cfg.MaxAttachBytes {
		p.send(errorRecord("quota", &q, id, 0))
		return
	}
	var oldTotal, oldArrived int
	err = e.store.db.QueryRow("SELECT total,arrived_at FROM attachments WHERE scope=? AND aid=?", id, aid).Scan(&oldTotal, &oldArrived)
	if err == nil && int64(oldTotal) != total {
		p.send(errorRecord("malformed", &q, id, 0))
		return
	}
	if err != nil && err != sql.ErrNoRows {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	var oldCID, oldData []byte
	err = e.store.db.QueryRow("SELECT cid,data FROM chunks WHERE scope=? AND aid=? AND idx=?", id, aid, idx).Scan(&oldCID, &oldData)
	if err == nil {
		if equalBytes(oldCID, cid) && equalBytes(oldData, data) {
			p.send(okRecord(q, nil))
		} else {
			p.send(errorRecord("conflict", &q, id, 0))
		}
		return
	}
	if err != sql.ErrNoRows {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	var scopeAttach int64
	if err := e.store.db.QueryRow("SELECT coalesce(sum(charged_bytes),0) FROM chunks WHERE scope=?", id).Scan(&scopeAttach); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	for scopeAttach+charge > e.cfg.MaxAttachBytes {
		var oldest []byte
		err := e.store.db.QueryRow("SELECT aid FROM attachments WHERE scope=? ORDER BY arrived_at,aid LIMIT 1", id).Scan(&oldest)
		if err == sql.ErrNoRows {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		if err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if equalBytes(oldest, aid) {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
		if err := e.expireAttachment(id, oldest, s.TTLMS, s.MaxFrames, nowMS); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if err := e.store.db.QueryRow("SELECT coalesce(sum(charged_bytes),0) FROM chunks WHERE scope=?", id).Scan(&scopeAttach); err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
	}
	if e.cfg.MaxBytes > 0 {
		pinned, err := e.pinnedBytes()
		if err != nil {
			p.send(errorRecord("internal", &q, id, 0))
			return
		}
		if pinned+charge > e.cfg.MaxBytes {
			p.send(errorRecord("quota", &q, id, 0))
			return
		}
	}
	if err := e.insertChunk(id, aid, int(idx), int(total), cid, data, charge, s, nowMS); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	e.counters.ChunksStored.Add(1)
	if err := e.enforceWatermark(); err != nil {
		p.send(errorRecord("internal", &q, id, 0))
		return
	}
	p.send(okRecord(q, nil))
}

func (e *Engine) insertChunk(scope, aid []byte, idx, total int, cid, data []byte, charge int64, s scopeState, now int64) error {
	return e.tx(func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT OR IGNORE INTO attachments(scope,aid,total,arrived_at) VALUES(?,?,?,?)", scope, aid, total, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO chunks(scope,aid,idx,cid,data,charged_bytes) VALUES(?,?,?,?,?,?)", scope, aid, idx, cid, data, charge)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE scopes SET last_active=? WHERE scope=?", now, scope)
		return err
	})
}

func (e *Engine) touchScope(scope []byte, now int64) error {
	_, err := e.store.db.Exec("UPDATE scopes SET last_active=? WHERE scope=?", now, scope)
	return err
}

func (e *Engine) handleScopeError(p *Peer, q int64, id []byte, err error) {
	var pe protocolError
	if errors.As(err, &pe) {
		if pe.code == "rate" {
			e.refuseRate(p, &q, id, pe.retryMS, pe.strike)
		} else {
			p.send(errorRecord(pe.code, &q, id, pe.retryMS))
		}
		return
	}
	p.send(errorRecord("internal", &q, id, 0))
}

func (e *Engine) expireScope(id []byte, s scopeState, now int64) error {
	framesChanged := false
	err := e.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT blob_id FROM frames WHERE scope=? AND arrived_at+?<=?", id, s.TTLMS, now)
		if err != nil {
			return err
		}
		var expired [][]byte
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, v)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, blob := range expired {
			if _, err := tx.Exec("DELETE FROM frames WHERE scope=? AND blob_id=?", id, blob); err != nil {
				return err
			}
			if err := e.addTombstoneTx(tx, id, blob, now+s.TTLMS, s.MaxFrames); err != nil {
				return err
			}
			framesChanged = true
		}
		rows, err = tx.Query("SELECT aid FROM attachments WHERE scope=? AND arrived_at+?<=?", id, s.TTLMS, now)
		if err != nil {
			return err
		}
		var aids [][]byte
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			aids = append(aids, v)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, aid := range aids {
			if err := e.expireAttachmentTx(tx, id, aid, now, s.TTLMS, s.MaxFrames); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM tombstones WHERE scope=? AND expires_at<=?", id, now); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM attachment_tombstones WHERE scope=? AND expires_at<=?", id, now); err != nil {
			return err
		}
		return nil
	})
	if err == nil && framesChanged {
		_ = e.broadcastDigest(id, nil)
	}
	return err
}

func (e *Engine) expireAttachment(scope, aid []byte, ttl int64, maxFrames int, now int64) error {
	return e.tx(func(tx *sql.Tx) error { return e.expireAttachmentTx(tx, scope, aid, now, ttl, maxFrames) })
}

func (e *Engine) expireAttachmentTx(tx *sql.Tx, scope, aid []byte, now, ttl int64, maxFrames int) error {
	var sequence int64
	if err := tx.QueryRow("SELECT next_value FROM sequence WHERE id=1").Scan(&sequence); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE sequence SET next_value=? WHERE id=1", sequence+1); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM attachments WHERE scope=? AND aid=?", scope, aid); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT INTO attachment_tombstones(scope,aid,expires_at,sequence) VALUES(?,?,?,?) ON CONFLICT(scope,aid) DO UPDATE SET expires_at=excluded.expires_at,sequence=excluded.sequence", scope, aid, now+ttl, sequence); err != nil {
		return err
	}
	cap := max(2*maxFrames, 1024)
	rows, err := tx.Query("SELECT aid FROM attachment_tombstones WHERE scope=? ORDER BY sequence DESC LIMIT -1 OFFSET ?", scope, cap)
	if err != nil {
		return err
	}
	var remove [][]byte
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			return err
		}
		remove = append(remove, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, value := range remove {
		if _, err := tx.Exec("DELETE FROM attachment_tombstones WHERE scope=? AND aid=?", scope, value); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) applyFrameBounds(id []byte, b bounds, now int64) error {
	s, ok, err := e.getScope(id)
	if err != nil || !ok {
		return err
	}
	return e.tx(func(tx *sql.Tx) error {
		rows, err := tx.Query("SELECT blob_id FROM frames WHERE scope=? AND arrived_at+?<=?", id, b.TTLMS, now)
		if err != nil {
			return err
		}
		var expired [][]byte
		for rows.Next() {
			var v []byte
			if err := rows.Scan(&v); err != nil {
				rows.Close()
				return err
			}
			expired = append(expired, v)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, blob := range expired {
			if _, err := tx.Exec("DELETE FROM frames WHERE scope=? AND blob_id=?", id, blob); err != nil {
				return err
			}
			if err := e.addTombstoneTx(tx, id, blob, now+b.TTLMS, b.MaxFrames); err != nil {
				return err
			}
		}
		var count int
		if err := tx.QueryRow("SELECT count(*) FROM frames WHERE scope=?", id).Scan(&count); err != nil {
			return err
		}
		for count > b.MaxFrames {
			var blob []byte
			if err := tx.QueryRow("SELECT blob_id FROM frames WHERE scope=? ORDER BY arrived_at,blob_id LIMIT 1", id).Scan(&blob); err != nil {
				return err
			}
			if _, err := tx.Exec("DELETE FROM frames WHERE scope=? AND blob_id=?", id, blob); err != nil {
				return err
			}
			if err := e.addTombstoneTx(tx, id, blob, now+b.TTLMS, b.MaxFrames); err != nil {
				return err
			}
			count--
		}
		if !s.Commons {
			if _, err := tx.Exec("UPDATE scopes SET max_frames=?,ttl_ms=?,max_blob=?,last_active=? WHERE scope=?", b.MaxFrames, b.TTLMS, b.MaxBlob, now, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *Engine) pinnedBytes() (int64, error) {
	var total int64
	err := e.store.db.QueryRow("SELECT coalesce(sum(length(f.data)),0) FROM frames f JOIN scopes s ON s.scope=f.scope WHERE s.is_commons=1").Scan(&total)
	if err != nil {
		return 0, err
	}
	var attachments int64
	err = e.store.db.QueryRow("SELECT coalesce(sum(c.charged_bytes),0) FROM chunks c JOIN scopes s ON s.scope=c.scope WHERE s.is_commons=1").Scan(&attachments)
	return total + attachments, err
}

func (e *Engine) totalBytes() (int64, error) {
	var frames, attachments int64
	if err := e.store.db.QueryRow("SELECT coalesce(sum(length(data)),0) FROM frames").Scan(&frames); err != nil {
		return 0, err
	}
	if err := e.store.db.QueryRow("SELECT coalesce(sum(charged_bytes),0) FROM chunks").Scan(&attachments); err != nil {
		return 0, err
	}
	return frames + attachments, nil
}

func (e *Engine) enforceWatermark() error {
	if e.cfg.MaxBytes <= 0 {
		return nil
	}
	total, err := e.totalBytes()
	if err != nil {
		return err
	}
	if total <= e.cfg.MaxBytes {
		return nil
	}
	// Return to 90% after crossing the hard cap, matching the official reference's
	// hysteresis and avoiding repeated eviction for every small write.
	lowWater := e.cfg.MaxBytes / 10 * 9
	for {
		total, err = e.totalBytes()
		if err != nil {
			return err
		}
		if total <= lowWater {
			return nil
		}
		var id []byte
		err = e.store.db.QueryRow("SELECT scope FROM scopes WHERE is_commons=0 ORDER BY last_active,created_at LIMIT 1").Scan(&id)
		if err == sql.ErrNoRows {
			return errors.New("storage watermark exceeded by pinned commons")
		}
		if err != nil {
			return err
		}
		if err := e.tx(func(tx *sql.Tx) error {
			_, err := tx.Exec("DELETE FROM scopes WHERE scope=? AND is_commons=0", id)
			return err
		}); err != nil {
			return err
		}
		e.counters.ScopesShed.Add(1)
		// Subscribers retain their connection-local subscription and receive an empty anchor.
		for p := range e.peers {
			if b, ok := p.subs[string(id)]; ok {
				d := e.digestForForgotten(id, b)
				p.send(d)
			}
		}
	}
}

func (e *Engine) Sweep() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UnixMilli()
	rows, err := e.store.db.Query("SELECT scope,max_frames,ttl_ms,max_blob,is_commons,created_at,last_active FROM scopes")
	if err != nil {
		return err
	}
	var scopes []scopeState
	for rows.Next() {
		var s scopeState
		var commons int
		if err := rows.Scan(&s.ID, &s.MaxFrames, &s.TTLMS, &s.MaxBlob, &commons, &s.CreatedAt, &s.LastActive); err != nil {
			rows.Close()
			return err
		}
		s.Commons = commons != 0
		scopes = append(scopes, s)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, s := range scopes {
		if err := e.expireScope(s.ID, s, now); err != nil {
			return err
		}
	}
	if _, err := e.store.db.Exec("DELETE FROM pow_cache WHERE day<?", PowDay(now)-1); err != nil {
		return err
	}
	for key, b := range e.newScopes {
		if b.last.Before(e.now().Add(-time.Hour)) {
			delete(e.newScopes, key)
		}
	}
	return e.enforceWatermark()
}

func (e *Engine) Stats() map[string]int64 {
	total, _ := e.totalBytes()
	var scopes int64
	_ = e.store.db.QueryRow("SELECT count(*) FROM scopes").Scan(&scopes)
	return map[string]int64{
		"connections":       e.counters.Connections.Load(),
		"connections_total": e.counters.ConnectionsTotal.Load(),
		"records":           e.counters.Records.Load(),
		"pushes":            e.counters.Pushes.Load(),
		"events":            e.counters.Events.Load(),
		"pow_verified":      e.counters.PowVerified.Load(),
		"rate_limited":      e.counters.RateLimited.Load(),
		"scopes_shed":       e.counters.ScopesShed.Load(),
		"chunks_stored":     e.counters.ChunksStored.Load(),
		"egress_bytes":      e.counters.EgressBytes.Load(),
		"scopes":            scopes, "live_bytes": total,
	}
}

func (e *Engine) digestFor(id []byte) (record, error) {
	s, ok, err := e.getScope(id)
	if err != nil || !ok {
		return nil, err
	}
	return e.currentDigest(id, s)
}

func (e *Engine) String() string { return "knit spool engine" }

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var difference byte
	for i := range a {
		difference |= a[i] ^ b[i]
	}
	return difference == 0
}

func normalizeAddress(remote string, trustProxy bool, forwarded string) (string, error) {
	if trustProxy && forwarded != "" {
		parts := strings.Split(forwarded, ",")
		candidate := strings.TrimSpace(parts[len(parts)-1])
		if ip := net.ParseIP(candidate); ip != nil {
			return ip.String(), nil
		}
		return "", errors.New("proxy address is malformed")
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", errors.New("remote address is malformed")
	}
	return ip.String(), nil
}

func sortedScopeKeys(values map[string]bounds) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func dayBytes(day uint64) []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], day); return b[:] }
