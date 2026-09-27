package spool

import (
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
