package spool

import (
	"crypto/sha256"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type differentialPeer struct {
	conn *websocket.Conn
}

func TestDifferentialAgainstOfficialKotlinReference(t *testing.T) {
	localURL := os.Getenv("KNIT_LOCAL_URL")
	referenceURL := os.Getenv("KNIT_REFERENCE_URL")
	if localURL == "" || referenceURL == "" {
		t.Skip("set KNIT_LOCAL_URL and KNIT_REFERENCE_URL to run the official reference differential")
	}
	local := openDifferentialPeer(t, localURL)
	reference := openDifferentialPeer(t, referenceURL)
	defer local.conn.Close()
	defer reference.conn.Close()
	localHello := readDifferentialRecord(t, local)
	referenceHello := readDifferentialRecord(t, reference)
	if localHello["t"] != "hello" || referenceHello["t"] != "hello" {
		t.Fatalf("handshake records: local=%#v reference=%#v", localHello, referenceHello)
	}
	clientHello := record{"t": "hello", "v": int64(1)}
	writeDifferentialRecord(t, local, clientHello)
	writeDifferentialRecord(t, reference, clientHello)

	seed := sha256.Sum256([]byte(fmt.Sprintf("knit-spool-diff-%d", time.Now().UnixNano())))
	scope := seed[:]
	bounds := map[string]any{"maxFrames": int64(2), "ttlMs": int64(60_000), "maxBlob": int64(1024)}
	sub := record{"t": "sub", "q": int64(1), "subs": []any{record{"scope": scope, "bounds": bounds, "futureBound": true}}}
	proofRefusal := compareDifferentialThroughQ(t, local, reference, sub, 1, "new-scope proof-of-work refusal")
	if len(proofRefusal) != 1 || proofRefusal[0]["code"] != "pow" {
		t.Fatalf("expected the same PoW refusal from both spools: %#v", proofRefusal)
	}
	day := uint64(PowDay(time.Now().UnixMilli()))
	nonce, ok := MinePow(scope, day, 8)
	if !ok {
		t.Fatal("could not mine the differential proof-of-work fixture")
	}
	sub["q"] = int64(2)
	entry := sub["subs"].([]any)[0].(record)
	entry["pow"] = record{"d": int64(day), "n": int64(nonce)}
	writeDifferentialRecord(t, local, sub)
	writeDifferentialRecord(t, reference, sub)
	localSub := readDifferentialRecord(t, local)
	referenceSub := readDifferentialRecord(t, reference)
	if localSub["t"] != "digest" || referenceSub["t"] != "digest" {
		t.Fatalf("subscribe did not establish both scopes: Go=%#v Reference=%#v", localSub, referenceSub)
	}
	compareDifferential(t, "subscribe", []record{localSub}, []record{referenceSub})

	frames := [][]byte{[]byte("reference frame one"), []byte("reference frame two"), []byte("reference frame three")}
	for i, data := range frames {
		blob := sha256.Sum256(data)
		q := int64(i + 3)
		request := record{"t": "push", "q": q, "scope": scope, "blobId": blob[:], "data": data, "futureField": "ignored"}
		compareDifferentialThroughQ(t, local, reference, request, q, "push and oldest-frame eviction")
		if i == 2 {
			compareDifferential(t, "eviction digest after acknowledgement",
				[]record{readDifferentialRecord(t, local)}, []record{readDifferentialRecord(t, reference)})
		}
	}
	ids := make([][]byte, 0, 3)
	for _, data := range frames {
		h := sha256.Sum256(data)
		ids = append(ids, h[:])
	}
	list := record{"t": "list", "q": int64(6), "scope": scope}
	listed := compareDifferentialThroughQ(t, local, reference, list, 6, "list and tombstones")
	if len(listed) != 1 || listed[0]["t"] != "list" {
		t.Fatalf("list transcript contains unexpected records: %#v", listed)
	}
	missing := sha256.Sum256([]byte("not stored"))
	pull := record{"t": "pull", "q": int64(7), "scope": scope, "blobIds": [][]byte{ids[1], missing[:]}}
	compareDifferentialThroughQ(t, local, reference, pull, 7, "pull and missing ids")

	badID := make([]byte, 32)
	badID[0] = 0x5a
	badPush := record{"t": "push", "q": int64(8), "scope": scope, "blobId": badID, "data": []byte("valid bytes, wrong id")}
	compareDifferentialThroughQ(t, local, reference, badPush, 8, "push digest rejection")

	aidHash := sha256.Sum256(append([]byte("attachment-"), seed[:]...))
	chunk := []byte("reference attachment chunk")
	cid := sha256.Sum256(chunk)
	aput := record{"t": "aput", "q": int64(9), "scope": scope, "aid": aidHash[:], "idx": int64(0), "total": int64(2), "cid": cid[:], "data": chunk}
	compareDifferentialThroughQ(t, local, reference, aput, 9, "attachment first write")
	compareDifferentialThroughQ(t, local, reference, aputWithQ(aput, 10), 10, "attachment idempotent retry")
	other := []byte("different attachment chunk")
	otherCID := sha256.Sum256(other)
	conflict := record{"t": "aput", "q": int64(11), "scope": scope, "aid": aidHash[:], "idx": int64(0), "total": int64(2), "cid": otherCID[:], "data": other}
	compareDifferentialThroughQ(t, local, reference, conflict, 11, "attachment first-write conflict")
	compareDifferentialThroughQ(t, local, reference,
		record{"t": "ahave", "q": int64(12), "scope": scope, "aid": aidHash[:]}, 12, "attachment presence")
	compareDifferentialThroughQ(t, local, reference,
		record{"t": "aget", "q": int64(13), "scope": scope, "aid": aidHash[:], "from": int64(0), "n": int64(8)}, 13, "attachment chunk read")

	// Unknown record types are silent; the following list must be the only response.
	unknown := record{"t": "future_record", "q": int64(99), "payload": []byte("ignored")}
	writeDifferentialRecord(t, local, unknown)
	writeDifferentialRecord(t, reference, unknown)
	last := compareDifferentialThroughQ(t, local, reference,
		record{"t": "list", "q": int64(14), "scope": scope}, 14, "unknown-record ignore")
	if len(last) != 1 || last[0]["t"] != "list" {
		t.Fatalf("unknown record produced a response: %#v", last)
	}
}

func aputWithQ(source record, q int64) record {
	copy := make(record, len(source))
	for key, value := range source {
		copy[key] = value
	}
	copy["q"] = q
	return copy
}

func openDifferentialPeer(t *testing.T, url string) *differentialPeer {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		if response != nil {
			t.Fatalf("connect to %s: %v (HTTP %s)", url, err, response.Status)
		}
		t.Fatalf("connect to %s: %v", url, err)
	}
	conn.SetReadLimit(256 * 1024)
	return &differentialPeer{conn: conn}
}

func writeDifferentialRecord(t *testing.T, peer *differentialPeer, value record) {
	t.Helper()
	data, err := encodeRecord(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("write differential record: %v", err)
	}
}

func readDifferentialRecord(t *testing.T, peer *differentialPeer) record {
	t.Helper()
	_ = peer.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := peer.conn.ReadMessage()
	if err != nil {
		t.Fatalf("read differential record: %v", err)
	}
	if kind != websocket.BinaryMessage {
		t.Fatalf("differential response was WebSocket message type %d", kind)
	}
	value, err := decodeRecord(data, 256*1024)
	if err != nil {
		t.Fatalf("decode differential response: %v", err)
	}
	return value
}

func compareDifferentialThroughQ(t *testing.T, local, reference *differentialPeer, request record, q int64, label string) []record {
	t.Helper()
	writeDifferentialRecord(t, local, request)
	writeDifferentialRecord(t, reference, request)
	readThroughQ := func(peer *differentialPeer) []record {
		var transcript []record
		for len(transcript) < 8 {
			response := readDifferentialRecord(t, peer)
			transcript = append(transcript, response)
			if got, ok := response.integer("q"); ok && got == q {
				return transcript
			}
		}
		t.Fatalf("%s exceeded response transcript cap", label)
		return nil
	}
	localTranscript := readThroughQ(local)
	referenceTranscript := readThroughQ(reference)
	compareDifferential(t, label, localTranscript, referenceTranscript)
	return localTranscript
}

func compareDifferential(t *testing.T, label string, local, reference []record) {
	t.Helper()
	if !reflect.DeepEqual(local, reference) {
		t.Fatalf("%s differs from official Kotlin reference:\nGo:        %#v\nReference: %#v", label, local, reference)
	}
}
