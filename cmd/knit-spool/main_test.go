package main

import (
	"bytes"
	"strings"
	"testing"

	spool "github.com/plaonn/knit-spool-go"
)

func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(args, strings.NewReader(stdin), &stdout, &stderr)
	return stdout.String(), stderr.String(), err
}

func field(t *testing.T, out, key string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if value, ok := strings.CutPrefix(line, key); ok {
			return value
		}
	}
	t.Fatalf("no %q in %q", key, out)
	return ""
}

func TestCommonsInviteWithoutPublicURL(t *testing.T) {
	t.Setenv("SPOOL_PUBLIC_URL", "")
	out, errOut, err := runCLI(t, "", "commons-invite")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "link:") || !strings.Contains(errOut, "SPOOL_PUBLIC_URL") {
		t.Fatalf("stdout %q stderr %q", out, errOut)
	}
	_, id, err := spool.DecodeCommonsInvite(field(t, out, "invite: "))
	if err != nil || field(t, out, "SPOOL_COMMONS_ID=") != spool.HexID(id[:]) {
		t.Fatalf("invite and id disagree: %q, %v", out, err)
	}
}

func TestCommonsInvitePrintsLinkForTheSameSecret(t *testing.T) {
	t.Setenv("SPOOL_PUBLIC_URL", "wss://home.example.org/spool/v1")
	t.Setenv("SPOOL_TOKEN", "fixture-token")
	t.Setenv("SPOOL_COMMONS_NAME", "Home")
	out, errOut, err := runCLI(t, "", "commons-invite")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut, "access token") {
		t.Fatalf("no credential warning: %q", errOut)
	}
	secret, _, err := spool.DecodeCommonsInvite(field(t, out, "invite: "))
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := spool.MintRelayInvite(spool.RelayInviteSettings{
		PublicURL: "wss://home.example.org/spool/v1", Token: "fixture-token", Name: "Home"}, secret)
	if err != nil || field(t, out, "link: ") != want {
		t.Fatalf("link does not carry the printed invite's secret: %q, %v", out, err)
	}
}

func TestCommonsInviteLinkFromStdin(t *testing.T) {
	t.Setenv("SPOOL_PUBLIC_URL", "wss://home.example.org/spool/v1")
	t.Setenv("SPOOL_TOKEN", "fixture-token")
	t.Setenv("SPOOL_COMMONS_NAME", "Home")
	out, _, err := runCLI(t, "knit-commons:v1:ChEYHyYtNDtCSVBXXmVsc3qBiI-WnaSrsrnAx87V3OM\n", "commons-invite", "--link-from-stdin")
	if err != nil {
		t.Fatal(err)
	}
	// The fullInvite golden vector from Knit docs/RELAY_INVITE.md §5.
	if got := field(t, out, "link: "); got != "https://getknit.app/r#pGF2AWF1eC93c3M6Ly9ob21lLmV4YW1wbGUub3JnL3Nwb29sL3YxP2s9Zml4dHVyZS10b2tlbmFjWCAKERgfJi00O0JJUFdeZWxzeoGIj5adpKuyucDHztXc42FuZEhvbWU" {
		t.Fatalf("link %s", got)
	}

	if _, _, err := runCLI(t, "not an invite\n", "commons-invite", "--link-from-stdin"); err == nil {
		t.Fatal("accepted a malformed invite")
	}
	t.Setenv("SPOOL_PUBLIC_URL", "")
	if _, _, err := runCLI(t, "", "commons-invite", "--link-from-stdin"); err == nil || !strings.Contains(err.Error(), "SPOOL_PUBLIC_URL") {
		t.Fatalf("missing public URL: %v", err)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"check", "x"}, {"commons-invite", "--other"}, {"run", "x"}, {"nope"}} {
		if _, _, err := runCLI(t, "", args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
