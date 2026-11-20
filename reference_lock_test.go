package spool

import (
	"bufio"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// readReferenceLock parses knit-reference.lock, the single source of the upstream pins.
func readReferenceLock(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open("knit-reference.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("knit-reference.lock: malformed line %q", line)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

func TestReferenceLockMatchesCodeAndDocs(t *testing.T) {
	lock := readReferenceLock(t)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	for _, key := range []string{"SPEC_COMMIT", "REFERENCE_COMMIT"} {
		if !sha.MatchString(lock[key]) {
			t.Fatalf("%s must be a full commit SHA, got %q", key, lock[key])
		}
	}
	if got := strconv.Itoa(ProtocolVersion); lock["PROTOCOL_VERSION"] != got {
		t.Fatalf("PROTOCOL_VERSION %q in knit-reference.lock, but ProtocolVersion is %s", lock["PROTOCOL_VERSION"], got)
	}

	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SPEC_REPO", "SPEC_COMMIT", "SPEC_REVISION", "REFERENCE_REPO", "REFERENCE_COMMIT"} {
		if lock[key] == "" || !strings.Contains(string(readme), lock[key]) {
			t.Errorf("README.md does not mention %s=%s from knit-reference.lock", key, lock[key])
		}
	}

	// The pins must come from the lock file, not be repeated where they can drift apart.
	hardcoded := regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows/upstream-drift.yml", "scripts/verify-reference.sh"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if match := hardcoded.Find(data); match != nil {
			t.Errorf("%s hardcodes commit %s; read it from knit-reference.lock", path, match)
		}
	}
}
