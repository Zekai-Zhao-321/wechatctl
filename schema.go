package main

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
)

type schemaColumn struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Annotation string `json:"annotation,omitempty"`
}

type schemaTable struct {
	Name    string         `json:"name"`
	Shards  []string       `json:"shards,omitempty"`
	Columns []schemaColumn `json:"columns"`
}

type schemaOutput struct {
	Domain    string        `json:"domain"`
	Inspected string        `json:"inspected,omitempty"`
	Note      string        `json:"note,omitempty"`
	Tables    []schemaTable `json:"tables"`
}

func cmdSchema(args []string) {
	if err := runSchema(args, os.Stdout); err != nil {
		fatal("schema: %v", err)
	}
}

func runSchema(args []string, output io.Writer) error {
	defaultKeys, err := defaultKeyStorePath()
	if err != nil {
		return fmt.Errorf("resolve key store: %w", err)
	}
	fs := flag.NewFlagSet("schema", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, "usage: wcctl schema [-db DOMAIN] [-chat USERNAME] [-json] [-user USER] [-keys PATH]")
		fs.PrintDefaults()
	}
	domainName := fs.String("db", "messages", "database domain: "+domainList())
	chat := fs.String("chat", "", "resolve the Msg_<md5(username)> table for one conversation")
	jsonOutput := fs.Bool("json", false, "print the schema as JSON")
	userName := fs.String("user", "", "WeChat user in the key store")
	keyStorePath := fs.String("keys", defaultKeys, "path to keys.json")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *chat != "" && flagProvided(fs, "db") {
		return fmt.Errorf("-db cannot be combined with -chat; conversation tables live in the messages domain")
	}

	store, err := readKeyStore(*keyStorePath)
	if err != nil {
		return err
	}
	selectedName, user, err := selectStoredUser(store, *userName)
	if err != nil {
		return err
	}

	if *chat != "" {
		result, err := schemaForChat(user, *chat)
		if err != nil {
			return fmt.Errorf("user %q: %w", selectedName, err)
		}
		return printSchema(output, result, *jsonOutput)
	}

	domain, err := resolveDomain(*domainName)
	if err != nil {
		return err
	}
	databases, err := domainDatabases(user, domain)
	if err != nil {
		return fmt.Errorf("user %q: %w", selectedName, err)
	}
	// Shards share one schema, so the shared tables are read from the first one
	// and the inspected shard is named rather than left implicit.
	inspected := databases[0]
	tables, err := listSchemaTables(inspected, domain.sharded)
	if err != nil {
		return fmt.Errorf("user %q: %w", selectedName, err)
	}
	result := schemaOutput{Domain: domain.name, Inspected: inspected.Shard, Tables: tables}
	if domain.sharded {
		result.Note = fmt.Sprintf("shared tables read from %s; per-conversation tables Msg_<md5(username)> are omitted, use -chat USERNAME to resolve one", inspected.Shard)
	}
	return printSchema(output, result, *jsonOutput)
}

func schemaForChat(user storedUser, chat string) (schemaOutput, error) {
	domain, err := resolveDomain("messages")
	if err != nil {
		return schemaOutput{}, err
	}
	databases, err := domainDatabases(user, domain)
	if err != nil {
		return schemaOutput{}, err
	}
	tableHash := md5.Sum([]byte(chat))
	tableName := "Msg_" + hex.EncodeToString(tableHash[:])

	var present []resolvedDatabase
	for _, database := range databases {
		exists, err := messageTableExists(database.Path, database.AESKey, tableName)
		if err != nil {
			return schemaOutput{}, err
		}
		if exists {
			present = append(present, database)
		}
	}
	if len(present) == 0 {
		return schemaOutput{}, fmt.Errorf("no messages table %s for %q in any shard", tableName, chat)
	}
	shards := make([]string, 0, len(present))
	for _, database := range present {
		shards = append(shards, database.Shard)
	}
	columns, err := tableColumns(present[0], tableName)
	if err != nil {
		return schemaOutput{}, err
	}
	return schemaOutput{
		Domain:    "messages",
		Inspected: present[0].Shard,
		Note:      fmt.Sprintf("conversation %q is table %s across shards %s; real_sender_id resolves via each shard's own Name2Id", chat, tableName, strings.Join(shards, ", ")),
		Tables:    []schemaTable{{Name: tableName, Shards: shards, Columns: columns}},
	}, nil
}

func listSchemaTables(database resolvedDatabase, skipMessageTables bool) ([]schemaTable, error) {
	var names []struct {
		Name string `json:"name"`
	}
	if err := queryDatabaseJSON(database.Path, database.AESKey,
		"SELECT name FROM sqlite_master WHERE type='table' ORDER BY name;", &names); err != nil {
		return nil, fmt.Errorf("list tables in %s: %w", database.Shard, err)
	}
	tables := make([]schemaTable, 0, len(names))
	for _, entry := range names {
		if skipMessageTables && isMessageTableName(entry.Name) {
			continue
		}
		columns, err := tableColumns(database, entry.Name)
		if err != nil {
			return nil, err
		}
		tables = append(tables, schemaTable{Name: entry.Name, Columns: columns})
	}
	return tables, nil
}

func tableColumns(database resolvedDatabase, table string) ([]schemaColumn, error) {
	var info []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	statement := fmt.Sprintf("PRAGMA table_info(%s);", quoteSQLIdentifier(table))
	if err := queryDatabaseJSON(database.Path, database.AESKey, statement, &info); err != nil {
		return nil, fmt.Errorf("inspect %s in %s: %w", table, database.Shard, err)
	}
	columns := make([]schemaColumn, 0, len(info))
	for _, entry := range info {
		columns = append(columns, schemaColumn{
			Name:       entry.Name,
			Type:       entry.Type,
			Annotation: columnAnnotation(entry.Name),
		})
	}
	return columns, nil
}

// columnAnnotation documents the WeChat quirks an author of raw SQL must know,
// so the agent does not have to rediscover them.
func columnAnnotation(name string) string {
	switch name {
	case "real_sender_id":
		return "per-shard rowid into this database's Name2Id; resolve to a wxid within the same shard"
	case "message_content", "source":
		return "may be zstd-compressed; check the WCDB_CT_<column> companion (4 = zstd)"
	case "WCDB_CT_message_content", "WCDB_CT_source":
		return "compression flag for the companion body column (4 = zstd)"
	case "local_type":
		return "packed: base type = local_type & 0xFFFFFFFF, appmsg subtype = local_type >> 32"
	case "create_time":
		return "unix epoch seconds"
	default:
		return ""
	}
}

// isMessageTableName reports whether a name is a per-conversation Msg_<md5>
// table (Msg_ followed by 32 lowercase hex characters).
func isMessageTableName(name string) bool {
	const prefix = "Msg_"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	digits := strings.TrimPrefix(name, prefix)
	if len(digits) != 32 {
		return false
	}
	for _, digit := range digits {
		if !((digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f')) {
			return false
		}
	}
	return true
}

func printSchema(output io.Writer, result schemaOutput, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if result.Note != "" {
		if _, err := fmt.Fprintf(output, "# %s\n\n", result.Note); err != nil {
			return err
		}
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	buffer := bufio.NewWriter(writer)
	for _, table := range result.Tables {
		header := table.Name
		if len(table.Shards) > 0 {
			header += " [" + strings.Join(table.Shards, ", ") + "]"
		}
		if _, err := fmt.Fprintln(buffer, header); err != nil {
			return err
		}
		for _, column := range table.Columns {
			line := "  " + column.Name + "\t" + column.Type
			if column.Annotation != "" {
				line += "\t" + column.Annotation
			}
			if _, err := fmt.Fprintln(buffer, line); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(buffer, ""); err != nil {
			return err
		}
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	return writer.Flush()
}
