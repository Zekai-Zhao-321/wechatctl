package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildFromSenderPreset(t *testing.T) {
	databases := []resolvedDatabase{{Path: "/db/message/message_fts.db", AESKey: testKey(), Shard: "message_fts.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "message_fts_v4_0_content"},
			{"name": "message_fts_v4_1_content"},
		}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	statement, err := buildFromSenderPreset(databases, []string{"wxid_test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`SELECT c0, c4, c5, c6 FROM "message_fts_v4_0_content"`,
		"UNION ALL",
		`WHERE c5 = (SELECT rowid FROM name2id WHERE username = 'wxid_test')`,
		"(SELECT username FROM name2id WHERE rowid = c4) AS conversation",
		"ORDER BY c6",
	} {
		if !strings.Contains(statement, fragment) {
			t.Fatalf("statement missing %q:\n%s", fragment, statement)
		}
	}
}

func TestBuildFromSenderPresetRequiresArgument(t *testing.T) {
	databases := []resolvedDatabase{{Path: "/db/message/message_fts.db", AESKey: testKey(), Shard: "message_fts.db"}}
	if _, err := buildFromSenderPreset(databases, nil); err == nil || !strings.Contains(err.Error(), "exactly one wxid") {
		t.Fatalf("expected argument error, got %v", err)
	}
}

func TestBuildFromSenderPresetEscapesQuotes(t *testing.T) {
	databases := []resolvedDatabase{{Path: "/db/message/message_fts.db", AESKey: testKey(), Shard: "message_fts.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"name": "message_fts_v4_0_content"}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	statement, err := buildFromSenderPreset(databases, []string{"a'b"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statement, "username = 'a''b'") {
		t.Fatalf("single quote not escaped: %s", statement)
	}
}

func TestPrintPresetListJSON(t *testing.T) {
	var output bytes.Buffer
	if err := printPresetList(&output, true); err != nil {
		t.Fatal(err)
	}
	var presets []map[string]any
	if err := json.Unmarshal(output.Bytes(), &presets); err != nil {
		t.Fatal(err)
	}
	if len(presets) == 0 || presets[0]["name"] != "from-sender" {
		t.Fatalf("unexpected preset list: %#v", presets)
	}
}

func TestBuildPresetRejectsUnknownName(t *testing.T) {
	user := storedUser{Databases: map[string]storedDatabase{}}
	_, _, err := buildPreset(user, "nope", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("expected unknown preset error, got %v", err)
	}
}

func TestBuildPresetResolvesFTSDomain(t *testing.T) {
	user := storedUser{Databases: map[string]storedDatabase{
		"/db/message/message_fts.db": {AESKey: hexKey()},
	}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"name": "message_fts_v4_0_content"}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	domain, statement, err := buildPreset(user, "from-sender", []string{"wxid_test"})
	if err != nil {
		t.Fatal(err)
	}
	if domain.name != "fts" || !strings.Contains(statement, "wxid_test") {
		t.Fatalf("unexpected preset resolution: domain=%q statement=%q", domain.name, statement)
	}
}
