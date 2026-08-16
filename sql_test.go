package main

import (
	"bytes"
	"flag"
	"fmt"
	"strings"
	"testing"
)

func testKey() []byte { return make([]byte, 32) }

func TestRunSQLHelp(t *testing.T) {
	var output bytes.Buffer
	if err := runSQL([]string{"-h"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.Contains(got, `wcctl sql [-db DOMAIN]`) {
		t.Fatalf("unexpected sql usage:\n%s", got)
	}
}

func TestCapStatement(t *testing.T) {
	got := capStatement("  SELECT 1 ;  ", 5)
	want := "WITH wcctl_query AS (\nSELECT 1\n)\nSELECT * FROM wcctl_query LIMIT 5"
	if got != want {
		t.Fatalf("capStatement = %q, want %q", got, want)
	}
}

func TestValidateSQLStatement(t *testing.T) {
	valid := []string{
		"SELECT 1",
		"SELECT id FROM sample WHERE name = 'a''b'",
		"SELECT id FROM sample WHERE name = ');'",
		"WITH q AS (SELECT 1) SELECT * FROM q",
		"SELECT 1 -- trailing comment",
		"SELECT /* inline */ 1",
		"SELECT 1;",
		"SELECT 1;   ",
	}
	for _, statement := range valid {
		if err := validateSQLStatement(statement); err != nil {
			t.Fatalf("validateSQLStatement(%q) = %v, want nil", statement, err)
		}
	}
	invalid := map[string]string{
		"SELECT id FROM sample) LIMIT 999999; --": "unbalanced",
		"SELECT (1":                   "unbalanced",
		"SELECT (1 -- unterminated":   "unbalanced",
		"SELECT 1; DROP TABLE sample": "one SQL statement",
		"SELECT 'unterminated":        "unterminated quoted",
		"SELECT 1 /* unterminated":    "unterminated block",
	}
	for statement, fragment := range invalid {
		err := validateSQLStatement(statement)
		if err == nil || !strings.Contains(err.Error(), fragment) {
			t.Fatalf("validateSQLStatement(%q) = %v, want error containing %q", statement, err, fragment)
		}
	}
}

func TestExecuteSQLRejectsCapBypass(t *testing.T) {
	domain := databaseDomain{name: "contact"}
	databases := []resolvedDatabase{{Path: "/db/contact/contact.db", AESKey: testKey(), Shard: "contact.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		t.Fatal("a cap-bypassing statement must never reach the database")
		return nil, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	_, err := executeSQL(databases, domain, "SELECT id FROM sample) LIMIT 999999; --", 10, false, false)
	if err == nil || !strings.Contains(err.Error(), "unbalanced") {
		t.Fatalf("expected the cap bypass to be rejected, got %v", err)
	}
}

func TestExecuteSQLDisclosesPerShardOrdering(t *testing.T) {
	domain, err := resolveDomain("messages")
	if err != nil {
		t.Fatal(err)
	}
	databases := []resolvedDatabase{
		{Path: "/db/message/message_0.db", AESKey: testKey(), Shard: "message_0.db"},
		{Path: "/db/message/message_1.db", AESKey: testKey(), Shard: "message_1.db"},
	}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"n": int64(1)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT n FROM t ORDER BY n DESC", 100, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Metadata.Ordering != "per-shard" {
		t.Fatalf("ordering = %q, want per-shard", envelope.Metadata.Ordering)
	}
	if len(envelope.Metadata.Warnings) == 0 || !strings.Contains(envelope.Metadata.Warnings[0], "not across the merged result") {
		t.Fatalf("missing per-shard ordering warning: %#v", envelope.Metadata.Warnings)
	}
	if envelope.Metadata.ShardRows["message_0.db"] != 1 || envelope.Metadata.ShardRows["message_1.db"] != 1 {
		t.Fatalf("shard_rows = %#v", envelope.Metadata.ShardRows)
	}
}

func TestExecuteSQLSingleDatabaseOrderingIsGlobal(t *testing.T) {
	domain, err := resolveDomain("fts")
	if err != nil {
		t.Fatal(err)
	}
	databases := []resolvedDatabase{{Path: "/db/message/message_fts.db", AESKey: testKey(), Shard: "message_fts.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"n": int64(1)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT n FROM t ORDER BY n", 100, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Metadata.Merged || envelope.Metadata.Ordering != "single-database" || len(envelope.Metadata.Warnings) != 0 {
		t.Fatalf("unexpected single-database metadata: %#v", envelope.Metadata)
	}
}

func TestIsMissingConversationTable(t *testing.T) {
	missingConversation := fmt.Errorf("execute query: no such table: Msg_0123456789abcdef0123456789abcdef (sqlite code 1)")
	if !isMissingConversationTable(missingConversation) {
		t.Fatal("expected a missing Msg_ table to be skippable")
	}
	missingOther := fmt.Errorf("execute query: no such table: Name2Id (sqlite code 1)")
	if isMissingConversationTable(missingOther) {
		t.Fatal("a missing non-conversation table must not be skipped")
	}
	if isMissingConversationTable(fmt.Errorf("disk I/O error")) {
		t.Fatal("unrelated errors must not be skipped")
	}
}

func TestPrintSQLResultRendersRowsWarningsAndExplain(t *testing.T) {
	var output bytes.Buffer
	envelope := sqlEnvelope{
		Status: "ok",
		Data:   []map[string]any{{"name": "alpha\nbeta", "n": int64(3), "blob": []byte{1, 2}, "missing": nil}},
		Metadata: sqlMetadata{
			Domain: "messages", Shards: []string{"message_0.db"}, Returned: 1, RowCap: 10,
			Warnings: []string{"careful"},
			Queries:  []sqlQuery{{DB: "message_0.db", SQL: "SELECT 1"}},
		},
	}
	if err := printSQLResult(&output, envelope); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	for _, fragment := range []string{"-- message_0.db", "SELECT 1", "NAME", "alpha beta", "<2 bytes>", "(1 rows from message_0.db)", "warning: careful"} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("output missing %q:\n%s", fragment, got)
		}
	}
}

func TestPrintSQLResultDryRunSkipsTable(t *testing.T) {
	var output bytes.Buffer
	envelope := sqlEnvelope{
		Status:   "dry_run",
		Data:     []map[string]any{},
		Metadata: sqlMetadata{Queries: []sqlQuery{{DB: "contact.db", SQL: "SELECT 1"}}},
	}
	if err := printSQLResult(&output, envelope); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "SELECT 1") || strings.Contains(got, "0 rows") {
		t.Fatalf("unexpected dry-run output:\n%s", got)
	}
}

func TestFormatSQLValueAndColumns(t *testing.T) {
	cases := map[string]any{
		"":          nil,
		"text":      "text",
		"42":        int64(42),
		"1.5":       1.5,
		"<3 bytes>": []byte{1, 2, 3},
	}
	for want, value := range cases {
		if got := formatSQLValue(value); got != want {
			t.Fatalf("formatSQLValue(%#v) = %q, want %q", value, got, want)
		}
	}
	columns := sqlResultColumns([]map[string]any{{"b": 1, "a": 2}, {"c": 3}})
	if strings.Join(columns, ",") != "a,b,c" {
		t.Fatalf("sqlResultColumns = %v", columns)
	}
}

func TestExecuteSQLMergesShardsAndTags(t *testing.T) {
	domain, err := resolveDomain("messages")
	if err != nil {
		t.Fatal(err)
	}
	databases := []resolvedDatabase{
		{Path: "/db/message/message_0.db", AESKey: testKey(), Shard: "message_0.db"},
		{Path: "/db/message/message_1.db", AESKey: testKey(), Shard: "message_1.db"},
	}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"id": int64(1)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT * FROM Name2Id", 100, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Metadata.Merged || len(envelope.Data) != 2 {
		t.Fatalf("expected 2 merged rows, got %#v", envelope.Metadata)
	}
	if envelope.Data[0]["_shard"] != "message_0.db" || envelope.Data[1]["_shard"] != "message_1.db" {
		t.Fatalf("shards not tagged: %#v", envelope.Data)
	}
	if envelope.Status != "ok" {
		t.Fatalf("status = %q", envelope.Status)
	}
}

func TestExecuteSQLSkipsMissingTableShard(t *testing.T) {
	domain, _ := resolveDomain("messages")
	databases := []resolvedDatabase{
		{Path: "/db/message/message_0.db", AESKey: testKey(), Shard: "message_0.db"},
		{Path: "/db/message/message_1.db", AESKey: testKey(), Shard: "message_1.db"},
	}
	conversation := messageTableFor("wxid_friend")
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		if strings.Contains(path, "message_1") {
			return nil, fmt.Errorf("execute query: no such table: %s (sqlite code 1)", conversation)
		}
		return []map[string]any{{"id": int64(1)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, fmt.Sprintf("SELECT * FROM %q", conversation), 100, false, false)
	if err != nil {
		t.Fatalf("expected missing-table shard to be skipped, got %v", err)
	}
	if len(envelope.Data) != 1 {
		t.Fatalf("expected 1 row from the surviving shard, got %d", len(envelope.Data))
	}
}

// A shard missing a shared table (rather than the per-conversation table) is a
// real error: skipping it would return a partial result labelled complete.
func TestExecuteSQLDoesNotSkipMissingSharedTable(t *testing.T) {
	domain, _ := resolveDomain("messages")
	databases := []resolvedDatabase{
		{Path: "/db/message/message_0.db", AESKey: testKey(), Shard: "message_0.db"},
		{Path: "/db/message/message_1.db", AESKey: testKey(), Shard: "message_1.db"},
	}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		if strings.Contains(path, "message_1") {
			return nil, fmt.Errorf("execute query: no such table: Name2Id (sqlite code 1)")
		}
		return []map[string]any{{"id": int64(1)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	if _, err := executeSQL(databases, domain, "SELECT * FROM Name2Id", 100, false, false); err == nil {
		t.Fatal("a missing shared table must surface as an error, not a silent partial result")
	}
}

func TestExecuteSQLTruncates(t *testing.T) {
	domain := databaseDomain{name: "contact"}
	databases := []resolvedDatabase{{Path: "/db/contact/contact.db", AESKey: testKey(), Shard: "contact.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{{"id": int64(1)}, {"id": int64(2)}, {"id": int64(3)}}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT id FROM contact", 2, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !envelope.Metadata.Truncated || envelope.Metadata.Returned != 2 || envelope.Status != "truncated" {
		t.Fatalf("expected truncation to 2 rows, got %#v", envelope.Metadata)
	}
}

func TestExecuteSQLDryRunDoesNotQuery(t *testing.T) {
	domain := databaseDomain{name: "contact"}
	databases := []resolvedDatabase{{Path: "/db/contact/contact.db", AESKey: testKey(), Shard: "contact.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		t.Fatal("dry-run must not execute the query")
		return nil, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT 1", 100, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "dry_run" || len(envelope.Data) != 0 || len(envelope.Metadata.Queries) != 1 {
		t.Fatalf("unexpected dry-run envelope: %#v", envelope)
	}
	if !strings.Contains(envelope.Metadata.Queries[0].SQL, "LIMIT 101") {
		t.Fatalf("dry-run SQL missing cap: %q", envelope.Metadata.Queries[0].SQL)
	}
}

func TestExecuteSQLEmpty(t *testing.T) {
	domain := databaseDomain{name: "contact"}
	databases := []resolvedDatabase{{Path: "/db/contact/contact.db", AESKey: testKey(), Shard: "contact.db"}}
	original := querySQLCipher
	querySQLCipher = func(path string, key []byte, statement string, immutable bool) ([]map[string]any, error) {
		return []map[string]any{}, nil
	}
	t.Cleanup(func() { querySQLCipher = original })

	envelope, err := executeSQL(databases, domain, "SELECT id FROM contact WHERE 0", 100, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Status != "empty" || len(envelope.Data) != 0 {
		t.Fatalf("expected empty status, got %#v", envelope)
	}
}

func TestReadSQLArgument(t *testing.T) {
	got, err := readSQLArgument([]string{"  SELECT 1  "}, strings.NewReader(""))
	if err != nil || got != "SELECT 1" {
		t.Fatalf("positional query = %q, err %v", got, err)
	}
	got, err = readSQLArgument([]string{"-"}, strings.NewReader("  SELECT 2\n"))
	if err != nil || got != "SELECT 2" {
		t.Fatalf("stdin query = %q, err %v", got, err)
	}
	if _, err := readSQLArgument(nil, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected a missing-query error, got %v", err)
	}
	if _, err := readSQLArgument([]string{"SELECT 1", "extra"}, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("expected an unexpected-argument error, got %v", err)
	}
}

func TestFlagProvided(t *testing.T) {
	fs := flag.NewFlagSet("sql", flag.ContinueOnError)
	fs.String("db", "messages", "")
	if err := fs.Parse([]string{"-db", "contact"}); err != nil {
		t.Fatal(err)
	}
	if !flagProvided(fs, "db") || flagProvided(fs, "limit") {
		t.Fatal("flagProvided did not distinguish a set flag from a default")
	}
}
