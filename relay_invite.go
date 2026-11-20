package spool

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"
	"unicode/utf16"
)

// The relay invite (Knit docs/RELAY_INVITE.md, layout version 1) hands a newcomer one link that adds
// this relay, with its access token, and joins a commons room.
const (
	relayInviteLinkPrefix = "https://getknit.app/r#"
	relayInviteMaxURL     = 256
	relayInviteMaxName    = 32
	relayInviteMaxBody    = 512
	relayInvitePath       = "/spool/v1"
)

// RelayInviteSettings are the inputs `knit-spool commons-invite` reads to mint a relay invite link.
type RelayInviteSettings struct {
	PublicURL string // wss://host[:port]/spool/v1, as clients reach this spool; empty disables the link
	Token     string // SPOOL_TOKEN, appended as ?k= so the link admits its holder
	Name      string // SPOOL_COMMONS_NAME, the room name the link shows
}

// LoadRelayInviteSettings reads SPOOL_PUBLIC_URL, SPOOL_TOKEN and SPOOL_COMMONS_NAME without the rest
// of the service configuration, so an invite can be minted before SPOOL_COMMONS_ID exists.
func LoadRelayInviteSettings() (RelayInviteSettings, error) {
	s := RelayInviteSettings{
		PublicURL: os.Getenv("SPOOL_PUBLIC_URL"),
		Token:     os.Getenv("SPOOL_TOKEN"),
		Name:      os.Getenv("SPOOL_COMMONS_NAME"),
	}
	if s.PublicURL != "" {
		if _, err := RelayInviteURL(s.PublicURL, s.Token); err != nil {
			return RelayInviteSettings{}, err
		}
	}
	return s, nil
}

// ValidatePublicURL checks SPOOL_PUBLIC_URL: a wss:// URL with the spool path and nothing else. The
// token is not part of it; RelayInviteURL appends SPOOL_TOKEN.
func ValidatePublicURL(raw string) error {
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return errors.New("SPOOL_PUBLIC_URL must not contain whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("SPOOL_PUBLIC_URL is not a URL: %w", err)
	}
	if u.Scheme != "wss" {
		return errors.New("SPOOL_PUBLIC_URL must use wss:// (released Knit clients refuse ws://)")
	}
	if u.Host == "" || u.Hostname() == "" || u.User != nil {
		return errors.New("SPOOL_PUBLIC_URL must name a host and no user information")
	}
	if u.Path != relayInvitePath || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return fmt.Errorf("SPOOL_PUBLIC_URL must be wss://host[:port]%s with no query or fragment; the token comes from SPOOL_TOKEN", relayInvitePath)
	}
	return nil
}

// RelayInviteURL is the invite's `u`: the public URL plus `?k=<token>` for a private spool.
func RelayInviteURL(publicURL, token string) (string, error) {
	if err := ValidatePublicURL(publicURL); err != nil {
		return "", err
	}
	u := publicURL
	if token != "" {
		u += "?k=" + url.QueryEscape(token)
	}
	if len(u) > relayInviteMaxURL {
		return "", fmt.Errorf("relay invite URL is %d characters; Knit accepts at most %d", len(u), relayInviteMaxURL)
	}
	return u, nil
}

// RelayInviteName returns the room name an invite may carry, or "" with a reason when the name has to
// be left out: the layout allows one line of at most 32 characters.
func RelayInviteName(name string) (string, string) {
	if name == "" {
		return "", ""
	}
	if strings.IndexFunc(name, func(r rune) bool { return r == '\n' || r == '\r' || unicode.IsControl(r) }) >= 0 {
		return "", "SPOOL_COMMONS_NAME is not a single line, so the link carries no room name"
	}
	// Knit measures strings in UTF-16 units.
	if n := len(utf16.Encode([]rune(name))); n > relayInviteMaxName {
		return "", fmt.Sprintf("SPOOL_COMMONS_NAME is %d characters, over the link's %d, so the link carries no room name", n, relayInviteMaxName)
	}
	return name, ""
}

// EncodeRelayInvite mints `https://getknit.app/r#<base64url(Body)>` with Body { v: 1, u, c?, n? } in
// the house CBOR (definite lengths, keys in layout order, absent fields omitted). This daemon never
// writes the ticket fields `i` and `k`.
func EncodeRelayInvite(u string, secret []byte, name string) (string, error) {
	if secret != nil && len(secret) != 32 {
		return "", errors.New("commons secret must be 32 bytes")
	}
	type field struct {
		key   string
		value any
	}
	fields := []field{{"v", 1}, {"u", u}}
	if secret != nil {
		fields = append(fields, field{"c", secret})
	}
	if name != "" {
		fields = append(fields, field{"n", name})
	}
	var body bytes.Buffer
	writeCBORHead(&body, 5, uint64(len(fields)))
	for _, f := range fields {
		if err := encodeValue(&body, f.key, ""); err != nil {
			return "", err
		}
		if err := encodeValue(&body, f.value, ""); err != nil {
			return "", err
		}
	}
	if body.Len() > relayInviteMaxBody {
		return "", fmt.Errorf("relay invite body is %d bytes; Knit accepts at most %d", body.Len(), relayInviteMaxBody)
	}
	return relayInviteLinkPrefix + base64.RawURLEncoding.EncodeToString(body.Bytes()), nil
}

// MintRelayInvite builds the link for a commons secret from the settings. It returns the link and any
// warning about fields left out.
func MintRelayInvite(s RelayInviteSettings, secret []byte) (string, string, error) {
	u, err := RelayInviteURL(s.PublicURL, s.Token)
	if err != nil {
		return "", "", err
	}
	name, warning := RelayInviteName(s.Name)
	link, err := EncodeRelayInvite(u, secret, name)
	return link, warning, err
}
