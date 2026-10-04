package spool

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func assertStoreSynchronous(t *testing.T, s *Store, want int) {
	t.Helper()
	var got int
	// Read the store's connection: synchronous is not a persistent file setting.
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("PRAGMA synchronous = %d, want %d", got, want)
	}
}

func TestStoreSQLiteSynchronous(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode string
		want int
	}{
		{"default", DefaultConfig().SQLiteSynchronous, 2},
		{"omitted", "", 2},
		{"FULL", "FULL", 2},
		{"NORMAL", "NORMAL", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testConfig(t)
			c.DataPath = filepath.Join(t.TempDir(), "spool.db")
			c.SQLiteSynchronous = tc.mode
			e := newTestEngine(t, c)
			assertStoreSynchronous(t, e.store, tc.want)
			var journal string
			if err := e.store.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
				t.Fatalf("journal_mode = %q, error = %v", journal, err)
			}
		})
	}
}

func TestStoreSQLiteSynchronousReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.db")
	// Keep the exact legacy function type as well as its FULL behavior.
	var legacyOpen func(string) (*Store, error) = OpenStore
	s, err := legacyOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			_ = s.Close()
		}
	})
	assertStoreSynchronous(t, s, 2)
	if _, err := s.db.Exec("UPDATE sequence SET next_value=42 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Same-file reopen, repeated NORMAL, rollback, and omitted-field compatibility.
	for _, mode := range []string{"NORMAL", "NORMAL", "FULL", "NORMAL", ""} {
		c := testConfig(t)
		c.DataPath = path
		c.SQLiteSynchronous = mode
		e := newTestEngine(t, c)
		want := 2
		if mode == "NORMAL" {
			want = 1
		}
		assertStoreSynchronous(t, e.store, want)
		var next int64
		if err := e.store.db.QueryRow("SELECT next_value FROM sequence WHERE id=1").Scan(&next); err != nil || next != 42 {
			t.Fatalf("reopen %q lost stored state: next=%d error=%v", mode, next, err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// A legacy caller must also restore FULL after a NORMAL store closes.
	s, err = openStore(path, "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = legacyOpen(path)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreSynchronous(t, s, 2)
}

func TestInvalidSQLiteSynchronousDoesNotMutateStore(t *testing.T) {
	for _, opener := range []string{"engine", "store"} {
		t.Run(opener, func(t *testing.T) {
			open := func(path string) {
				t.Helper()
				const invalid = "NORMAL; PRAGMA user_version=99"
				var err error
				if opener == "engine" {
					c := testConfig(t)
					c.DataPath = path
					c.SQLiteSynchronous = invalid
					var e *Engine
					e, err = NewEngine(c)
					if e != nil {
						_ = e.Close()
						t.Fatal("invalid mode opened an engine")
					}
				} else {
					var s *Store
					s, err = openStore(path, invalid)
					if s != nil {
						_ = s.Close()
						t.Fatal("invalid mode opened a store")
					}
				}
				if err == nil || err.Error() != "SPOOL_SQLITE_SYNCHRONOUS must be FULL or NORMAL" {
					t.Fatalf("expected mode rejection before I/O, got %v", err)
				}
			}
			dir := filepath.Join(t.TempDir(), "new")
			path := filepath.Join(dir, "spool.db")
			open(path)
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("invalid mode created a directory: %v", err)
			}
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			for target, mode := range map[string]os.FileMode{dir: 0o755, path: 0o644} {
				if err := os.Chmod(target, mode); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			open(path)
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("invalid mode changed the existing database: %v", err)
			}
			for target, want := range map[string]os.FileMode{dir: 0o755, path: 0o644} {
				st, err := os.Stat(target)
				if err != nil || st.Mode().Perm() != want {
					t.Fatalf("invalid mode changed permissions: %v %v", st, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != "spool.db" {
				t.Fatalf("invalid mode changed database sidecars: %v %v", entries, err)
			}
		})
	}
}
