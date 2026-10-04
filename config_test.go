package spool

import (
	"os"
	"strings"
	"testing"
)

func TestConfigStringRedactsCredentials(t *testing.T) {
	c := DefaultConfig()
	c.Token = "connect-secret"
	c.TokenNext = "next-secret"
	c.MetricsToken = "metrics-secret"
	got := c.String()
	for _, secret := range []string{c.Token, c.TokenNext, c.MetricsToken} {
		if strings.Contains(got, secret) {
			t.Fatalf("config diagnostic disclosed a credential: %q", got)
		}
	}
	if !strings.Contains(got, "token=true") {
		t.Fatalf("config diagnostic should reveal credential presence: %q", got)
	}
}

func TestConfigSQLiteSynchronous(t *testing.T) {
	if got := DefaultConfig().SQLiteSynchronous; got != "FULL" {
		t.Fatalf("default SQLite mode = %q, want FULL", got)
	}
	for _, mode := range []string{"", "FULL", "NORMAL"} {
		t.Run("mode="+mode, func(t *testing.T) {
			c := DefaultConfig()
			c.SQLiteSynchronous = mode
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			want := mode
			if want == "" {
				want = "FULL"
			}
			if got := c.String(); !strings.Contains(got, "sqliteSynchronous="+want) {
				t.Fatalf("effective mode missing from diagnostic: %s", got)
			}
			if !c.AttachmentsEnabled() || c.MaxAttachBytes != 16*1024*1024 || c.MaxAChunk != 49_221 || c.MaxAget != 32 {
				t.Fatal("SQLite mode must preserve attachment defaults")
			}
		})
	}
	for _, mode := range []string{"OFF", "EXTRA", "0", "1", "2", "normal", " FULL ", "NORMAL; DROP TABLE frames", "synthetic-secret\n"} {
		t.Run("invalid="+mode, func(t *testing.T) {
			c := DefaultConfig()
			c.SQLiteSynchronous = mode
			if err := c.Validate(); err == nil || err.Error() != "SPOOL_SQLITE_SYNCHRONOUS must be FULL or NORMAL" {
				t.Fatalf("expected redacted validation error, got %v", err)
			}
			want := strings.Replace(DefaultConfig().String(), "sqliteSynchronous=FULL", "sqliteSynchronous=invalid", 1)
			if got := c.String(); got != want {
				t.Fatalf("invalid input must not be echoed: %q", got)
			}
		})
	}
}

func TestLoadConfigSQLiteSynchronous(t *testing.T) {
	const key = "SPOOL_SQLITE_SYNCHRONOUS"
	// Setenv registers restoration of the original value before testing absence.
	t.Setenv(key, "FULL")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig()
	if err != nil || c.SQLiteSynchronous != "FULL" {
		t.Fatalf("unset mode = %q, error = %v", c.SQLiteSynchronous, err)
	}
	for _, mode := range []string{"FULL", "NORMAL", "", "OFF", "EXTRA", "1", "normal", " NORMAL ", "NORMAL; PRAGMA user_version=99", "synthetic-secret\n"} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv(key, mode)
			c, err := LoadConfig()
			if mode == "FULL" || mode == "NORMAL" {
				if err != nil || c.SQLiteSynchronous != mode {
					t.Fatalf("mode = %q, error = %v", c.SQLiteSynchronous, err)
				}
			} else if err == nil || err.Error() != "SPOOL_SQLITE_SYNCHRONOUS must be FULL or NORMAL" {
				t.Fatalf("expected redacted validation error, got %v", err)
			}
		})
	}
}

func TestLoadConfigRejectsAmbiguousDataLocation(t *testing.T) {
	t.Setenv("SPOOL_DATA_DIR", t.TempDir())
	t.Setenv("SPOOL_DATA_PATH", t.TempDir()+"/custom.db")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Fatalf("expected an explicit data path conflict, got %v", err)
	}
}

func TestConfigRequiresTLSForPublicListener(t *testing.T) {
	c := DefaultConfig()
	c.ListenAddr = "0.0.0.0:9470"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("expected cleartext public bind rejection, got %v", err)
	}
	c.TLSCertFile = "/etc/knit-spool/cert.pem"
	c.TLSKeyFile = "/etc/knit-spool/key.pem"
	if err := c.Validate(); err != nil {
		t.Fatalf("public listener with paired TLS files rejected: %v", err)
	}
}
