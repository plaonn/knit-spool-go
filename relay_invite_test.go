package spool

import (
	"strings"
	"testing"
)

// Golden vectors from Knit docs/RELAY_INVITE.md §5.
func TestRelayInviteGoldenVectors(t *testing.T) {
	secret := protocolFixture(32, 10)
	invite, _, err := EncodeCommonsInvite(secret)
	if err != nil || invite != "knit-commons:v1:ChEYHyYtNDtCSVBXXmVsc3qBiI-WnaSrsrnAx87V3OM" {
		t.Fatalf("fixture secret spells %q, %v", invite, err)
	}

	u, err := RelayInviteURL("wss://home.example.org/spool/v1", "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	full, err := EncodeRelayInvite(u, secret, "Home")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://getknit.app/r#pGF2AWF1eC93c3M6Ly9ob21lLmV4YW1wbGUub3JnL3Nwb29sL3YxP2s9Zml4dHVyZS10b2tlbmFjWCAKERgfJi00O0JJUFdeZWxzeoGIj5adpKuyucDHztXc42FuZEhvbWU"; full != want {
		t.Errorf("fullInvite\n got %s\nwant %s", full, want)
	}

	u, err = RelayInviteURL("wss://lax.spool.getknit.app/spool/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	bare, err := EncodeRelayInvite(u, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://getknit.app/r#omF2AWF1eCR3c3M6Ly9sYXguc3Bvb2wuZ2V0a25pdC5hcHAvc3Bvb2wvdjE"; bare != want {
		t.Errorf("bareInvite\n got %s\nwant %s", bare, want)
	}
}

func TestRelayInviteURLValidation(t *testing.T) {
	for _, bad := range []string{
		"ws://home.example.org/spool/v1",
		"https://home.example.org/spool/v1",
		"wss://home.example.org/",
		"wss://home.example.org/spool/v1/",
		"wss://home.example.org/spool/v1?k=inline",
		"wss://home.example.org/spool/v1#x",
		"wss://user@home.example.org/spool/v1",
		"wss:///spool/v1",
		"wss://home.example.org/spool/v1 ",
		"wss://home.example.org/spool/v1\n",
	} {
		if err := ValidatePublicURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	for _, good := range []string{"wss://home.example.org/spool/v1", "wss://home.example.org:8443/spool/v1", "wss://[2001:db8::1]/spool/v1"} {
		if err := ValidatePublicURL(good); err != nil {
			t.Errorf("refused %q: %v", good, err)
		}
	}
	u, err := RelayInviteURL("wss://h.example/spool/v1", "a b&c=d")
	if err != nil || u != "wss://h.example/spool/v1?k=a+b%26c%3Dd" {
		t.Fatalf("token must be query-escaped: %q, %v", u, err)
	}
	if _, err := RelayInviteURL("wss://h.example/spool/v1", strings.Repeat("t", 240)); err == nil {
		t.Fatal("URL over 256 characters accepted")
	}
}

func TestRelayInviteNameAndSize(t *testing.T) {
	for name, want := range map[string]string{"": "", "Home": "Home", strings.Repeat("é", 32): strings.Repeat("é", 32)} {
		if got, warning := RelayInviteName(name); got != want || warning != "" {
			t.Errorf("RelayInviteName(%q) = %q, %q", name, got, warning)
		}
	}
	for _, name := range []string{"two\nlines", strings.Repeat("x", 33), strings.Repeat("😀", 17)} {
		if got, warning := RelayInviteName(name); got != "" || warning == "" {
			t.Errorf("RelayInviteName(%q) kept %q without a warning", name, got)
		}
	}
	if _, err := EncodeRelayInvite("wss://h.example/spool/v1", make([]byte, 31), ""); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := EncodeRelayInvite(strings.Repeat("u", 600), nil, ""); err == nil {
		t.Fatal("body over 512 bytes accepted")
	}

	link, warning, err := MintRelayInvite(RelayInviteSettings{
		PublicURL: "wss://home.example.org/spool/v1", Token: "fixture-token", Name: strings.Repeat("x", 40),
	}, protocolFixture(32, 10))
	if err != nil || warning == "" || !strings.HasPrefix(link, relayInviteLinkPrefix) {
		t.Fatalf("mint with long name: %q, %q, %v", link, warning, err)
	}
}

func TestLoadConfigValidatesPublicURL(t *testing.T) {
	t.Setenv("SPOOL_PUBLIC_URL", "ws://home.example.org/spool/v1")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "SPOOL_PUBLIC_URL") {
		t.Fatalf("LoadConfig accepted a ws:// public URL: %v", err)
	}
	t.Setenv("SPOOL_PUBLIC_URL", "wss://home.example.org/spool/v1")
	if _, err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
}
