package spool

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ProtocolVersion = 1
	ScopeIDBytes    = 32
	DigestBytes     = 8
	DefaultTTLMS    = int64(48 * time.Hour / time.Millisecond)
	DefaultFrames   = 400
	DefaultBlob     = 64 * 1024
	DefaultAChunk   = 49_221
)

// CommonsConfig pins the one operator-owned shared scope advertised by a spool.
type CommonsConfig struct {
	ScopeID   [ScopeIDBytes]byte
	Name      string
	MaxFrames int
	TTLMS     int64
	MaxBlob   int
	Attach    bool
	PushRate  int
}

// Config contains immutable listener/store settings and protocol limits.
// Credentials are intentionally excluded from String and diagnostic output.
type Config struct {
	ListenAddr        string
	TLSCertFile       string
	TLSKeyFile        string
	DataPath          string
	SourceURL         string
	Token             string
	TokenNext         string
	MetricsToken      string
	MaxBlob           int
	MaxRecord         int
	MaxScopes         int
	MaxFramesCap      int
	MaxTTLMS          int64
	MaxPull           int
	PowBits           int
	MaxAttachBytes    int64
	MaxAChunk         int
	MaxAget           int
	MaxBytes          int64
	SweepInterval     time.Duration
	StatusInterval    time.Duration
	MaxConns          int
	MaxConnsPerIP     int
	RateRecords       int
	RatePushes        int
	RateNewScopes     int
	TrustProxy        bool
	RequireModeration bool
	Commons           *CommonsConfig
}

func DefaultConfig() Config {
	return Config{
		ListenAddr:     "127.0.0.1:9470",
		DataPath:       "./data/spool.db",
		SourceURL:      "https://github.com/plaonn/knit-spool-go",
		MaxBlob:        DefaultBlob,
		MaxRecord:      128 * 1024,
		MaxScopes:      64,
		MaxFramesCap:   1000,
		MaxTTLMS:       int64(7 * 24 * time.Hour / time.Millisecond),
		MaxPull:        64,
		PowBits:        20,
		MaxAttachBytes: 16 * 1024 * 1024,
		MaxAChunk:      DefaultAChunk,
		MaxAget:        32,
		MaxBytes:       256 * 1024 * 1024,
		SweepInterval:  time.Minute,
		StatusInterval: 5 * time.Minute,
		MaxConnsPerIP:  16,
		RateRecords:    50,
		RatePushes:     10,
		RateNewScopes:  6,
	}
}

// LoadConfig reads the documented SPOOL_* environment settings. Invalid values fail closed.
func LoadConfig() (Config, error) {
	c := DefaultConfig()
	setString := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	setInt := func(key string, dst *int) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%s must be an integer", key)
		}
		*dst = n
		return nil
	}
	setInt64 := func(key string, dst *int64) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("%s must be an integer", key)
		}
		*dst = n
		return nil
	}
	setBool := func(key string, dst *bool) error {
		v, ok := os.LookupEnv(key)
		if !ok {
			return nil
		}
		n, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s must be true or false", key)
		}
		*dst = n
		return nil
	}
	setDurationMS := func(key string, dst *time.Duration) error {
		var ms int64
		if err := setInt64(key, &ms); err != nil {
			return err
		}
		if _, ok := os.LookupEnv(key); ok {
			if ms < 0 || ms > int64((24*time.Hour)/time.Millisecond)*365 {
				return fmt.Errorf("%s is outside the supported range", key)
			}
			*dst = time.Duration(ms) * time.Millisecond
		}
		return nil
	}

	setString("SPOOL_LISTEN", &c.ListenAddr)
	setString("SPOOL_TLS_CERT", &c.TLSCertFile)
	setString("SPOOL_TLS_KEY", &c.TLSKeyFile)
	setString("SPOOL_DATA_PATH", &c.DataPath)
	setString("SPOOL_SOURCE_URL", &c.SourceURL)
	setString("SPOOL_TOKEN", &c.Token)
	setString("SPOOL_TOKEN_NEXT", &c.TokenNext)
	setString("SPOOL_METRICS_TOKEN", &c.MetricsToken)
	_, hasDataDir := os.LookupEnv("SPOOL_DATA_DIR")
	_, hasDataPath := os.LookupEnv("SPOOL_DATA_PATH")
	if hasDataDir && hasDataPath {
		return Config{}, errors.New("set only one of SPOOL_DATA_DIR and SPOOL_DATA_PATH")
	}
	if v, ok := os.LookupEnv("SPOOL_DATA_DIR"); ok {
		if strings.TrimSpace(v) == "" {
			return Config{}, errors.New("SPOOL_DATA_DIR cannot be empty")
		}
		c.DataPath = filepath.Join(v, "spool.db")
	}
	if err := setInt("SPOOL_MAX_BLOB", &c.MaxBlob); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_RECORD", &c.MaxRecord); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_SCOPES", &c.MaxScopes); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_FRAMES", &c.MaxFramesCap); err != nil {
		return Config{}, err
	}
	if err := setInt64("SPOOL_MAX_TTL_MS", &c.MaxTTLMS); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_PULL", &c.MaxPull); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_POW_BITS", &c.PowBits); err != nil {
		return Config{}, err
	}
	if err := setInt64("SPOOL_MAX_ATTACH_BYTES", &c.MaxAttachBytes); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_A_CHUNK", &c.MaxAChunk); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_AGET", &c.MaxAget); err != nil {
		return Config{}, err
	}
	if err := setInt64("SPOOL_MAX_BYTES", &c.MaxBytes); err != nil {
		return Config{}, err
	}
	if err := setDurationMS("SPOOL_SWEEP_MS", &c.SweepInterval); err != nil {
		return Config{}, err
	}
	if err := setDurationMS("SPOOL_STATUS_MS", &c.StatusInterval); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_CONNS", &c.MaxConns); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_MAX_CONNS_PER_IP", &c.MaxConnsPerIP); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_RATE_RECORDS", &c.RateRecords); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_RATE_PUSHES", &c.RatePushes); err != nil {
		return Config{}, err
	}
	if err := setInt("SPOOL_RATE_NEW_SCOPES", &c.RateNewScopes); err != nil {
		return Config{}, err
	}
	if err := setBool("SPOOL_TRUST_PROXY", &c.TrustProxy); err != nil {
		return Config{}, err
	}
	if err := setBool("SPOOL_REQUIRE_MODERATION", &c.RequireModeration); err != nil {
		return Config{}, err
	}
	var commonsID string
	setString("SPOOL_COMMONS_ID", &commonsID)
	if commonsID != "" {
		idBytes, err := hex.DecodeString(commonsID)
		if err != nil || len(idBytes) != ScopeIDBytes {
			return Config{}, errors.New("SPOOL_COMMONS_ID must be 64 lowercase or uppercase hex characters")
		}
		var id [ScopeIDBytes]byte
		copy(id[:], idBytes)
		cc := &CommonsConfig{ScopeID: id, MaxFrames: 500, TTLMS: int64(24 * time.Hour / time.Millisecond), MaxBlob: c.MaxBlob, PushRate: 20}
		setString("SPOOL_COMMONS_NAME", &cc.Name)
		if err := setInt("SPOOL_COMMONS_MAX_FRAMES", &cc.MaxFrames); err != nil {
			return Config{}, err
		}
		if err := setInt64("SPOOL_COMMONS_TTL_MS", &cc.TTLMS); err != nil {
			return Config{}, err
		}
		if err := setInt("SPOOL_COMMONS_MAX_BLOB", &cc.MaxBlob); err != nil {
			return Config{}, err
		}
		if err := setInt("SPOOL_COMMONS_RATE_PUSHES", &cc.PushRate); err != nil {
			return Config{}, err
		}
		if err := setBool("SPOOL_COMMONS_ATTACH", &cc.Attach); err != nil {
			return Config{}, err
		}
		c.Commons = cc
	} else if _, ok := os.LookupEnv("SPOOL_COMMONS_ATTACH"); ok {
		return Config{}, errors.New("SPOOL_COMMONS_ATTACH requires SPOOL_COMMONS_ID")
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.ListenAddr == "" || c.DataPath == "" {
		return errors.New("listen address and data path are required")
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		return errors.New("TLS certificate and key must be configured together")
	}
	if c.TLSCertFile == "" {
		host, _, err := net.SplitHostPort(c.ListenAddr)
		if err != nil {
			return errors.New("SPOOL_LISTEN must be host:port")
		}
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("a non-loopback listener requires TLS; use a local TLS-terminating proxy otherwise")
		}
	}
	if c.TokenNext != "" && (c.Token == "" || subtle.ConstantTimeCompare([]byte(c.Token), []byte(c.TokenNext)) == 1) {
		return errors.New("SPOOL_TOKEN_NEXT requires a different SPOOL_TOKEN")
	}
	if c.MaxBlob < 1 || c.MaxBlob > 16*1024*1024 {
		return errors.New("SPOOL_MAX_BLOB must be between 1 and 16777216")
	}
	if max(c.MaxBlob, c.MaxAChunk)+512 > c.MaxRecord || c.MaxRecord > 16*1024*1024 {
		return errors.New("SPOOL_MAX_RECORD must fit the largest payload plus record overhead and stay within 16 MiB")
	}
	if c.MaxScopes < 1 || c.MaxScopes > 65536 || c.MaxFramesCap < 1 || c.MaxFramesCap > 1_000_000 {
		return errors.New("scope and frame caps are outside supported bounds")
	}
	maxListBytes := 34*(c.MaxFramesCap+max(2*c.MaxFramesCap, 1024)) + 512
	if maxListBytes > c.MaxRecord {
		return errors.New("frame cap and tombstone budget must fit one list record")
	}
	if c.MaxTTLMS < 1 || c.MaxPull < 1 || c.MaxPull > 65536 || c.PowBits < 0 || c.PowBits > 24 {
		return errors.New("TTL, pull, or proof-of-work setting is outside supported bounds")
	}
	if c.MaxAttachBytes < 0 || c.MaxAttachBytes > 1<<40 || c.MaxAChunk < 1 || c.MaxAget < 1 || c.MaxAget > 65536 {
		return errors.New("attachment setting is outside supported bounds")
	}
	if c.MaxBytes < 0 || c.MaxConns < 0 || c.MaxConnsPerIP < 0 || c.RateRecords < 0 || c.RatePushes < 0 || c.RateNewScopes < 0 {
		return errors.New("watermark, capacity, and rate limits cannot be negative")
	}
	if c.SweepInterval < 0 || c.StatusInterval < 0 {
		return errors.New("sweep and status intervals cannot be negative")
	}
	if c.Commons != nil {
		cc := c.Commons
		if cc.MaxFrames < 1 || cc.MaxFrames > c.MaxFramesCap || cc.TTLMS < 1 || cc.TTLMS > c.MaxTTLMS || cc.MaxBlob < 1 || cc.MaxBlob > c.MaxBlob || cc.PushRate < 1 {
			return errors.New("commons limits must fit the spool hard caps")
		}
		if cc.Attach && c.MaxAttachBytes == 0 {
			return errors.New("commons attachments require attachment support")
		}
		if c.MaxBytes > 0 && int64(cc.MaxFrames)*int64(cc.MaxBlob) > c.MaxBytes {
			return errors.New("commons frame capacity cannot fit under SPOOL_MAX_BYTES")
		}
	}
	return nil
}

func (c Config) AttachmentsEnabled() bool { return c.MaxAttachBytes > 0 }

func (c Config) EffectiveMetricsToken() string {
	if c.MetricsToken != "" {
		return c.MetricsToken
	}
	return c.Token
}

func (c Config) String() string {
	commons := "off"
	if c.Commons != nil {
		commons = "on"
	}
	store := "persistent"
	if c.DataPath == ":memory:" {
		store = "memory"
	}
	return fmt.Sprintf("listen=%s maxBlob=%d maxRecord=%d maxScopes=%d maxFrames=%d maxTTLMS=%d powBits=%d attachments=%t commons=%s moderation=%t store=%s token=%t",
		c.ListenAddr, c.MaxBlob, c.MaxRecord, c.MaxScopes, c.MaxFramesCap, c.MaxTTLMS, c.PowBits,
		c.AttachmentsEnabled(), commons, c.RequireModeration, store, c.Token != "")
}
