package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
)

func hexKey() string { return hex.EncodeToString(make([]byte, 32)) }

func messageTableFor(chat string) string {
	sum := md5.Sum([]byte(chat))
	return "Msg_" + hex.EncodeToString(sum[:])
}

func TestRunSchemaHelp(t *testing.T) {
	var output bytes.Buffer
	if err := runSchema([]string{"-h"}, &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, "wcctl schema [-db DOMAIN] [-chat USERNAME] [-json] [-user USER] [-keys PATH]") {
		t.Fatalf("unexpected schema usage:\n%s", got)
	}
}

func TestIsMessageTableName(t *testing.T) {
	cases := map[string]bool{
		"Msg_0123456789abcdef0123456789abcdef": true,
		"Msg_short":                            false,
		"Msg_0123456789ABCDEF0123456789abcdef": false,
		"Name2Id":                              false,
		"Msg_0123456789abcdef0123456789abcdeg": false,
	}
	for name, want := range cases {
		if got := isMessageTableName(name); got != want {
			t.Fatalf("isMessageTableName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestColumnAnnotation(t *testing.T) {
	if got := columnAnnotation("real_sender_id"); !strings.Contains(got, "Name2Id") {
		t.Fatalf("real_sender_id annotation = %q", got)
	}
	if got := columnAnnotation("WCDB_CT_message_content"); !strings.Contains(got, "4 = zstd") {
		t.Fatalf("compression flag annotation = %q", got)
	}
	if got := columnAnnotation("local_type"); !strings.Contains(got, ">> 32") {
		t.Fatalf("local_type annotation = %q", got)
	}
	if got := columnAnnotation("something_else"); got != "" {
		t.Fatalf("unexpected annotation for unknown column: %q", got)
	}
}

func TestListSchemaTablesSkipsMessageTables(t *testing.T) {
	database := resolvedDatabase{Path: "/db/message/message_0.db", AESKey: testKey(), Shard: "message_0.db"}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		if strings.Contains(statement, "sqlite_master") {
			return []map[string]any{
				{"name": "Msg_0123456789abcdef0123456789abcdef"},
				{"name": "Name2Id"},
				{"name": "TimeStamp"},
			}, nil
		}
		// PRAGMA table_info(...)
		return []map[string]any{{"name": "rowid", "type": "INTEGER"}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	tables, err := listSchemaTables(database, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 {
		t.Fatalf("expected Msg_ table skipped, got %d tables: %#v", len(tables), tables)
	}
	for _, table := range tables {
		if isMessageTableName(table.Name) {
			t.Fatalf("Msg_ table was not skipped: %s", table.Name)
		}
	}
}

func TestSchemaForChatResolvesTable(t *testing.T) {
	user := storedUser{Databases: map[string]storedDatabase{
		"/db/message/message_0.db": {AESKey: hexKey()},
		"/db/message/message_1.db": {AESKey: hexKey()},
	}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		if strings.Contains(statement, "sqlite_master") {
			// Only message_0.db holds the conversation table.
			if strings.Contains(path, "message_0") {
				return []map[string]any{{"name": messageTableFor("wxid_friend")}}, nil
			}
			return []map[string]any{}, nil
		}
		return []map[string]any{
			{"name": "real_sender_id", "type": "INTEGER"},
			{"name": "message_content", "type": "BLOB"},
		}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	result, err := schemaForChat(user, "wxid_friend")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tables) != 1 || result.Tables[0].Name != messageTableFor("wxid_friend") {
		t.Fatalf("unexpected chat schema: %#v", result.Tables)
	}
	if strings.Join(result.Tables[0].Shards, ",") != "message_0.db" {
		t.Fatalf("expected only message_0.db shard, got %v", result.Tables[0].Shards)
	}
	if result.Tables[0].Columns[0].Annotation == "" {
		t.Fatalf("expected real_sender_id annotation, got none")
	}
}

func TestPrintSchemaTableAndJSON(t *testing.T) {
	result := schemaOutput{
		Domain:    "messages",
		Inspected: "message_0.db",
		Note:      "shared tables read from message_0.db",
		Tables: []schemaTable{{
			Name:    "Name2Id",
			Shards:  []string{"message_0.db"},
			Columns: []schemaColumn{{Name: "real_sender_id", Type: "INTEGER", Annotation: "per-shard rowid"}},
		}},
	}
	var table bytes.Buffer
	if err := printSchema(&table, result, false); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"# shared tables read from message_0.db", "Name2Id [message_0.db]", "real_sender_id", "per-shard rowid"} {
		if !strings.Contains(table.String(), fragment) {
			t.Fatalf("schema table missing %q:\n%s", fragment, table.String())
		}
	}
	var encoded bytes.Buffer
	if err := printSchema(&encoded, result, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded.String(), `"inspected": "message_0.db"`) {
		t.Fatalf("schema JSON missing inspected shard:\n%s", encoded.String())
	}
}
