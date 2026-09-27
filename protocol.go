package spool

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"

	"github.com/fxamacker/cbor/v2"
)

type record map[string]any

var (
	protocolDecoder cbor.DecMode
)

func init() {
	protocolDecoder, _ = (cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		MaxNestedLevels:  8,
		MaxArrayElements: 65_536,
		MaxMapPairs:      128,
		IntDec:           cbor.IntDecConvertSignedOrFail,
		DefaultMapType:   reflect.TypeOf(map[string]any{}),
	}).DecMode()
}

func encodeRecord(r record) ([]byte, error) {
	var out bytes.Buffer
	if err := encodeValue(&out, r, ""); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Kotlin's protocol codec emits record fields in their schema order. Encoding Go maps directly
// would randomize the wire order, so preserve that order for the v1 records and sort extensions.
func encodeValue(out *bytes.Buffer, value any, path string) error {
	switch v := value.(type) {
	case record:
		return encodeMap(out, map[string]any(v), path)
	case map[string]any:
		return encodeMap(out, v, path)
	case []byte:
		writeCBORHead(out, 2, uint64(len(v)))
		_, _ = out.Write(v)
	case string:
		writeCBORHead(out, 3, uint64(len(v)))
		_, _ = out.WriteString(v)
	case bool:
		if v {
			out.WriteByte(0xf5)
		} else {
			out.WriteByte(0xf4)
		}
	case nil:
		out.WriteByte(0xf6)
	case int:
		return encodeInteger(out, int64(v))
	case int8:
		return encodeInteger(out, int64(v))
	case int16:
		return encodeInteger(out, int64(v))
	case int32:
		return encodeInteger(out, int64(v))
	case int64:
		return encodeInteger(out, v)
	case uint:
		writeCBORHead(out, 0, uint64(v))
	case uint8:
		writeCBORHead(out, 0, uint64(v))
	case uint16:
		writeCBORHead(out, 0, uint64(v))
	case uint32:
		writeCBORHead(out, 0, uint64(v))
	case uint64:
		writeCBORHead(out, 0, v)
	case []any:
		writeCBORHead(out, 4, uint64(len(v)))
		childPath := ""
		if path == "subs" {
			childPath = "sub"
		}
		for _, item := range v {
			if err := encodeValue(out, item, childPath); err != nil {
				return err
			}
		}
	case [][]byte:
		writeCBORHead(out, 4, uint64(len(v)))
		for _, item := range v {
			if err := encodeValue(out, item, ""); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unsupported CBOR protocol value %T", value)
	}
	return nil
}

func encodeMap(out *bytes.Buffer, values map[string]any, path string) error {
	keys := orderedMapKeys(values, path)
	writeCBORHead(out, 5, uint64(len(keys)))
	for _, key := range keys {
		if err := encodeValue(out, key, ""); err != nil {
			return err
		}
		childPath := key
		if path == "sub" && key != "bounds" && key != "pow" {
			childPath = ""
		}
		if err := encodeValue(out, values[key], childPath); err != nil {
			return err
		}
	}
	return nil
}

func orderedMapKeys(values map[string]any, path string) []string {
	var ordered []string
	switch path {
	case "":
		switch values["t"] {
		case "hello":
			ordered = []string{"t", "v", "min", "limits", "powBits", "commons", "moderation"}
		case "sub":
			ordered = []string{"t", "q", "subs"}
		case "digest":
			ordered = []string{"t", "scope", "digest", "count", "full", "bounds"}
		case "list":
			ordered = []string{"t", "q", "scope", "blobIds", "tombstones"}
		case "pull":
			ordered = []string{"t", "q", "scope", "blobIds"}
		case "blob":
			ordered = []string{"t", "scope", "blobId", "data"}
		case "push":
			ordered = []string{"t", "q", "scope", "blobId", "data", "pow"}
		case "event":
			ordered = []string{"t", "scope", "blobId", "data"}
		case "ok":
			ordered = []string{"t", "q", "missing"}
		case "err":
			ordered = []string{"t", "code", "msg", "q", "scope", "retryMs"}
		case "ahave":
			ordered = []string{"t", "q", "scope", "aid"}
		case "ahas":
			ordered = []string{"t", "q", "scope", "aid", "total", "bits", "dead"}
		case "aget":
			ordered = []string{"t", "q", "scope", "aid", "from", "n"}
		case "achunk":
			ordered = []string{"t", "scope", "aid", "idx", "total", "cid", "data"}
		case "aput":
			ordered = []string{"t", "q", "scope", "aid", "idx", "total", "cid", "data", "pow"}
		}
	case "limits":
		ordered = []string{"maxBlob", "maxRecord", "maxScopes", "maxPull", "maxFramesCap", "maxTtlMs", "maxAttachBytes", "maxAChunk", "maxAget"}
	case "bounds":
		ordered = []string{"maxFrames", "ttlMs", "maxBlob"}
	case "commons":
		ordered = []string{"name", "maxFrames", "ttlMs", "maxBlob", "attach"}
	case "pow":
		ordered = []string{"n", "d"}
	case "sub":
		ordered = []string{"scope", "bounds", "pow"}
	}
	seen := make(map[string]struct{}, len(values))
	keys := make([]string, 0, len(values))
	for _, key := range ordered {
		if _, ok := values[key]; ok {
			keys = append(keys, key)
			seen[key] = struct{}{}
		}
	}
	var extras []string
	for key := range values {
		if _, ok := seen[key]; !ok {
			extras = append(extras, key)
		}
	}
	sort.Strings(extras)
	return append(keys, extras...)
}

func encodeInteger(out *bytes.Buffer, value int64) error {
	if value >= 0 {
		writeCBORHead(out, 0, uint64(value))
		return nil
	}
	if value == math.MinInt64 {
		writeCBORHead(out, 1, uint64(math.MaxInt64))
		return nil
	}
	writeCBORHead(out, 1, uint64(-1-value))
	return nil
}

func writeCBORHead(out *bytes.Buffer, major byte, value uint64) {
	prefix := major << 5
	switch {
	case value < 24:
		out.WriteByte(prefix | byte(value))
	case value <= math.MaxUint8:
		out.WriteByte(prefix | 24)
		out.WriteByte(byte(value))
	case value <= math.MaxUint16:
		out.WriteByte(prefix | 25)
		out.WriteByte(byte(value >> 8))
		out.WriteByte(byte(value))
	case value <= math.MaxUint32:
		out.WriteByte(prefix | 26)
		for shift := 24; shift >= 0; shift -= 8 {
			out.WriteByte(byte(value >> uint(shift)))
		}
	default:
		out.WriteByte(prefix | 27)
		for shift := 56; shift >= 0; shift -= 8 {
			out.WriteByte(byte(value >> uint(shift)))
		}
	}
}

func decodeRecord(data []byte, maxRecord int) (record, error) {
	if len(data) == 0 || len(data) > maxRecord {
		return nil, errors.New("record size outside the negotiated limit")
	}
	var r map[string]any
	if err := protocolDecoder.Unmarshal(data, &r); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("record must be a CBOR map")
	}
	return record(r), nil
}

func (r record) text(key string) (string, bool) {
	v, ok := r[key].(string)
	return v, ok
}

func (r record) bytes(key string) ([]byte, bool) {
	v, ok := r[key].([]byte)
	return v, ok
}

func (r record) integer(key string) (int64, bool) {
	switch v := r[key].(type) {
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int:
		return int64(v), true
	case uint64:
		if v <= uint64(^uint64(0)>>1) {
			return int64(v), true
		}
	case uint32:
		return int64(v), true
	case uint:
		return int64(v), true
	}
	return 0, false
}

func (r record) boolean(key string) (bool, bool) {
	v, ok := r[key].(bool)
	return v, ok
}

func (r record) array(key string) ([]any, bool) {
	v, ok := r[key].([]any)
	return v, ok
}

func (r record) nested(key string) (record, bool) {
	switch v := r[key].(type) {
	case map[string]any:
		return record(v), true
	case record:
		return v, true
	default:
		return nil, false
	}
}

func requireText(r record, key string) (string, error) {
	v, ok := r.text(key)
	if !ok || v == "" {
		return "", fmt.Errorf("%s must be non-empty text", key)
	}
	return v, nil
}

func requireBytes(r record, key string, n int) ([]byte, error) {
	v, ok := r.bytes(key)
	if !ok || (n >= 0 && len(v) != n) {
		return nil, fmt.Errorf("%s must be a %d-byte CBOR byte string", key, n)
	}
	return v, nil
}

func requireInt(r record, key string, min, max int64) (int64, error) {
	v, ok := r.integer(key)
	if !ok || v < min || v > max {
		return 0, fmt.Errorf("%s must be an integer in [%d,%d]", key, min, max)
	}
	return v, nil
}

func mustRecordType(r record) string {
	t, _ := r.text("t")
	return t
}

func digestRecord(scope []byte, digest uint64, count, maxFrames int, ttlMS int64, maxBlob int) record {
	var digestBytes [DigestBytes]byte
	for i := range digestBytes {
		digestBytes[DigestBytes-1-i] = byte(digest >> (8 * i))
	}
	return record{
		"t": "digest", "scope": scope, "digest": digestBytes[:], "count": count,
		"full":   count >= maxFrames,
		"bounds": record{"maxFrames": maxFrames, "ttlMs": ttlMS, "maxBlob": maxBlob},
	}
}

func okRecord(q int64, missing [][]byte) record {
	r := record{"t": "ok", "q": q}
	if missing != nil {
		r["missing"] = byteSlices(missing)
	}
	return r
}

func errorRecord(code string, q *int64, scope []byte, retryMS int64) record {
	r := record{"t": "err", "code": code}
	if q != nil {
		r["q"] = *q
	}
	if len(scope) == ScopeIDBytes {
		r["scope"] = append([]byte(nil), scope...)
	}
	if retryMS > 0 {
		r["retryMs"] = retryMS
	}
	return r
}

func byteSlices(values [][]byte) []any {
	result := make([]any, len(values))
	for i := range values {
		result[i] = values[i]
	}
	return result
}

func digestWire(value uint64) []byte {
	b := make([]byte, DigestBytes)
	for i := range b {
		b[DigestBytes-1-i] = byte(value >> (8 * i))
	}
	return b
}
