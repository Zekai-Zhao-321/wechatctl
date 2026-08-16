package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureAESKey mirrors the key used by internal/sqlcipher/testdata/fixture.db
// (bytes 0..31).
func fixtureAESKey() []byte {
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index)
	}
	return key
}

// TestSQLCommandIsReadOnly exercises the real sqlcipher query path (not the
// mock) through the sql command against an encrypted fixture: reads succeed,
// every write verb is rejected, and the database stays byte-identical.
func TestSQLCommandIsReadOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	databasePath := filepath.Join(root, "contact", "contact.db")
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("internal", "sqlcipher", "testdata", "fixture.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(root, "keys.json")
	store := keyStore{Users: map[string]storedUser{
		"user-a": {Databases: map[string]storedDatabase{
			databasePath: {AESKey: hex.EncodeToString(fixtureAESKey())},
		}},
	}}
	data, err := json.Marshal(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}

	// A read succeeds through the real query path.
	var output bytes.Buffer
	if err := runSQL([]string{"-db", "contact", "-keys", keyPath, "-json", "SELECT id, name FROM sample"}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("read query failed: %v", err)
	}
	var envelope sqlEnvelope
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data) != 1 || envelope.Data[0]["name"] != "alpha" {
		t.Fatalf("unexpected read result: %#v", envelope.Data)
	}

	// Every write verb is rejected by the command. These die in the row-cap
	// wrapper before SQLite prepares them; the read-only guards underneath are
	// asserted separately below.
	for _, write := range []string{
		"DELETE FROM sample",
		"UPDATE sample SET name = 'x'",
		"INSERT INTO sample (id) VALUES (2)",
		"DROP TABLE sample",
		"CREATE TABLE evil (x)",
		"ALTER TABLE sample RENAME TO renamed",
		"SELECT id FROM sample; DROP TABLE sample",
		"SELECT id FROM sample) LIMIT 999999; --",
	} {
		var discard bytes.Buffer
		if err := runSQL([]string{"-db", "contact", "-keys", keyPath, "-json", write}, strings.NewReader(""), &discard); err == nil {
			t.Fatalf("write statement was accepted: %q", write)
		}
	}

	// The guards themselves: an unwrapped write reaching sqlcipher.Query is
	// refused by SQLITE_OPEN_READONLY + query_only + sqlite3_stmt_readonly.
	for _, write := range []string{
		"DELETE FROM sample",
		"UPDATE sample SET name = 'x'",
		"DROP TABLE sample",
	} {
		if _, err := querySQLCipher(databasePath, fixtureAESKey(), write, false); err == nil {
			t.Fatalf("sqlcipher accepted a write statement: %q", write)
		}
	}

	// The database file is byte-identical: nothing leaked through.
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("database changed after write attempts")
	}
}
