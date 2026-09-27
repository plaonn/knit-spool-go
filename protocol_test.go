package spool

import (
	"encoding/hex"
	"testing"
)

func protocolFixture(n, seed int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte((i*7 + seed) & 0xff)
	}
	return data
}

func assertWireVector(t *testing.T, name string, value record, expected string) {
	t.Helper()
	got, err := encodeRecord(value)
	if err != nil {
		t.Fatalf("%s encode: %v", name, err)
	}
	if hex.EncodeToString(got) != expected {
		t.Errorf("%s wire vector mismatch\n got %s\nwant %s", name, hex.EncodeToString(got), expected)
	}
}

func TestOfficialProtocolRecordVectors(t *testing.T) {
	fixtureID := protocolFixture(32, 1)
	assertWireVector(t, "helloClient", record{"t": "hello", "v": 1}, "a261746568656c6c6f617601")
	assertWireVector(t, "helloSpool", record{
		"t": "hello", "v": 1, "min": 1,
		"limits":  record{"maxBlob": 65536, "maxRecord": 131072, "maxScopes": 64, "maxPull": 64, "maxFramesCap": 1000, "maxTtlMs": 604800000},
		"powBits": 20,
	}, "a561746568656c6c6f617601636d696e01666c696d697473a6676d6178426c6f621a00010000696d61785265636f72641a00020000696d617853636f7065731840676d617850756c6c18406c6d61784672616d65734361701903e8686d617854746c4d731a240c840067706f774269747314")
	assertWireVector(t, "sub", record{
		"t": "sub", "q": int64(1), "subs": []any{record{
			"scope": fixtureID, "bounds": record{"maxFrames": 400, "ttlMs": int64(172800000), "maxBlob": 65536},
			"pow": record{"n": int64(42), "d": int64(20680)},
		}},
	}, "a3617463737562617101647375627381a36573636f7065582001080f161d242b323940474e555c636a71787f868d949ba2a9b0b7bec5ccd3da66626f756e6473a3696d61784672616d65731901906574746c4d731a0a4cb800676d6178426c6f621a0001000063706f77a2616e182a61641950c8")
	assertWireVector(t, "digest", record{
		"t": "digest", "scope": fixtureID, "digest": protocolFixture(8, 2), "count": 3, "full": false,
		"bounds": record{"maxFrames": 400, "ttlMs": int64(172800000), "maxBlob": 65536},
	}, "a66174666469676573746573636f7065582001080f161d242b323940474e555c636a71787f868d949ba2a9b0b7bec5ccd3da6664696765737448020910171e252c3365636f756e74036466756c6cf466626f756e6473a3696d61784672616d65731901906574746c4d731a0a4cb800676d6178426c6f621a00010000")
	assertWireVector(t, "push", record{
		"t": "push", "q": int64(4), "scope": fixtureID, "blobId": protocolFixture(32, 3), "data": protocolFixture(48, 6), "pow": record{"n": int64(42), "d": int64(20680)},
	}, "a6617464707573686171046573636f7065582001080f161d242b323940474e555c636a71787f868d949ba2a9b0b7bec5ccd3da66626c6f6249645820030a11181f262d343b424950575e656c737a81888f969da4abb2b9c0c7ced5dc64646174615830060d141b222930373e454c535a61686f767d848b9299a0a7aeb5bcc3cad1d8dfe6edf4fb020910171e252c333a41484f63706f77a2616e182a61641950c8")
	assertWireVector(t, "okMissing", record{"t": "ok", "q": int64(3), "missing": []any{protocolFixture(32, 4)}}, "a36174626f6b617103676d697373696e67815820040b121920272e353c434a51585f666d747b828990979ea5acb3bac1c8cfd6dd")
	assertWireVector(t, "errScoped", record{"t": "err", "code": "tombstoned", "q": int64(4), "scope": fixtureID}, "a461746365727264636f64656a746f6d6273746f6e65646171046573636f7065582001080f161d242b323940474e555c636a71787f868d949ba2a9b0b7bec5ccd3da")
}

func TestOfficialDigestAndProofOfWorkVectors(t *testing.T) {
	if got := ScopeDigest([][]byte{protocolFixture(32, 11), protocolFixture(32, 12), protocolFixture(32, 13)}); digestWire(got) == nil || hex.EncodeToString(digestWire(got)) != "834b13d8dc060ce5" {
		t.Fatalf("digest vector got %x", digestWire(got))
	}
	if hex.EncodeToString(digestWire(0)) != "0000000000000000" {
		t.Fatal("empty digest is not zero")
	}
	const expected = "00b776b91276563998bb57f8f3f73a05e0d8afcd3dce8a2583d6d466aadb620e"
	h := PowHash(protocolFixture(32, 9), 20680, 8)
	if hex.EncodeToString(h[:]) != expected {
		t.Fatalf("PoW hash vector got %x", h)
	}
	if !VerifyPow(protocolFixture(32, 9), 20680, 8, 20_680*86_400_000, 8) {
		t.Fatal("official PoW stamp was rejected")
	}
	for n := uint64(0); n < 8; n++ {
		if VerifyPow(protocolFixture(32, 9), 20680, n, 20_680*86_400_000, 8) {
			t.Fatalf("nonce %d unexpectedly passes", n)
		}
	}
	if n, ok := MinePow(protocolFixture(32, 9), 20680, 8); !ok || n != 8 {
		t.Fatalf("miner got (%d,%t), want (8,true)", n, ok)
	}
}

func TestDecoderRejectsIndefiniteAndDuplicateMapKeys(t *testing.T) {
	if _, err := decodeRecord([]byte{0xbf, 0xff}, 128); err == nil {
		t.Fatal("accepted indefinite map")
	}
	if _, err := decodeRecord([]byte{0xa2, 0x61, 0x74, 0x61, 0x78, 0x61, 0x74, 0x61, 0x79}, 128); err == nil {
		t.Fatal("accepted duplicate key")
	}
}
