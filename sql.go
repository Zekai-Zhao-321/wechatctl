package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// defaultSQLRowCap bounds how many rows sql returns before it truncates, so a
// broad query cannot flood the caller's context. Raise it with -limit.
const defaultSQLRowCap = 1000

// perShardOrderWarning explains why a merged result is not globally ordered.
// A raw statement is opaque to wcctl, so ORDER BY and LIMIT are evaluated
// by SQLite inside each shard and the results are concatenated, never re-ranked.
const perShardOrderWarning = "rows are concatenated per shard: ORDER BY and LIMIT apply within each shard, not across the merged result. " +
	"For ranked or aggregated queries across conversations use -db fts (a single database) or a preset."

// sqlEnvelope is the structured result of the sql command. Existing verbs keep
// their bare-array output; sql is the new enveloped command.
type sqlEnvelope struct {
	Status   string           `json:"status"`
	Data     []map[string]any `json:"data"`
	Metadata sqlMetadata      `json:"metadata"`
}

type sqlMetadata struct {
	Domain    string         `json:"domain"`
	Shards    []string       `json:"shards"`
	Merged    bool           `json:"merged"`
	Ordering  string         `json:"ordering"`
	Returned  int            `json:"returned"`
	RowCap    int            `json:"row_cap"`
	Truncated bool           `json:"truncated"`
	ShardRows map[string]int `json:"shard_rows,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	Queries   []sqlQuery     `json:"queries,omitempty"`
}

type sqlQuery struct {
	DB  string `json:"db"`
	SQL string `json:"sql"`
}

func cmdSQL(args []string) {
	if err := runSQL(args, os.Stdin, os.Stdout); err != nil {
		fatal("sql: %v", err)
	}
}

func runSQL(args []string, input io.Reader, output io.Writer) error {
	defaultKeys, err := defaultKeyStorePath()
	if err != nil {
		return fmt.Errorf("resolve key store: %w", err)
	}
	fs := flag.NewFlagSet("sql", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() {
		fmt.Fprintln(output, `usage: wcctl sql [-db DOMAIN] [-limit N] [-json] [-explain] [-dry-run] [-user USER] [-keys PATH] "QUERY"
       wcctl sql -preset NAME [ARG ...]
       wcctl sql -list-presets`)
		fs.PrintDefaults()
	}
	domainName := fs.String("db", "messages", "database domain: "+domainList())
	limit := fs.Int("limit", defaultSQLRowCap, "maximum rows returned before truncation")
	presetName := fs.String("preset", "", "run a built-in preset instead of a raw query")
	listPresets := fs.Bool("list-presets", false, "list the built-in presets and exit")
	jsonOutput := fs.Bool("json", false, "print the result envelope as JSON")
	explain := fs.Bool("explain", false, "report the executed SQL alongside the result")
	dryRun := fs.Bool("dry-run", false, "report the SQL that would run without executing it")
	userName := fs.String("user", "", "WeChat user in the key store")
	keyStorePath := fs.String("keys", defaultKeys, "path to keys.json")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	if *listPresets {
		return printPresetList(output, *jsonOutput)
	}
	if *limit < 1 || *limit > 100000 {
		return fmt.Errorf("-limit must be between 1 and 100000")
	}
	if *presetName != "" && flagProvided(fs, "db") {
		return fmt.Errorf("-db cannot be combined with -preset; each preset selects its own domain")
	}

	store, err := readKeyStore(*keyStorePath)
	if err != nil {
		return err
	}
	selectedName, user, err := selectStoredUser(store, *userName)
	if err != nil {
		return err
	}

	var domain databaseDomain
	var statement string
	if *presetName != "" {
		domain, statement, err = buildPreset(user, *presetName, fs.Args())
	} else {
		domain, err = resolveDomain(*domainName)
		if err != nil {
			return err
		}
		statement, err = readSQLArgument(fs.Args(), input)
	}
	if err != nil {
		return err
	}

	databases, err := domainDatabases(user, domain)
	if err != nil {
		return fmt.Errorf("user %q: %w", selectedName, err)
	}
	envelope, err := executeSQL(databases, domain, statement, *limit, *explain, *dryRun)
	if err != nil {
		return fmt.Errorf("user %q: %w", selectedName, err)
	}
	if *jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(envelope)
	}
	return printSQLResult(output, envelope)
}

// flagProvided reports whether a flag was set on the command line, as opposed
// to carrying its default value.
func flagProvided(fs *flag.FlagSet, name string) bool {
	provided := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			provided = true
		}
	})
	return provided
}

// readSQLArgument reads the single positional query, or all of stdin when the
// argument is "-".
func readSQLArgument(args []string, input io.Reader) (string, error) {
	if len(args) == 0 {
		return "", fmt.Errorf(`a SQL query argument is required (or "-" to read it from stdin)`)
	}
	if len(args) > 1 {
		return "", fmt.Errorf("unexpected argument %q", args[1])
	}
	if args[0] == "-" {
		data, err := io.ReadAll(input)
		if err != nil {
			return "", fmt.Errorf("read query from stdin: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return strings.TrimSpace(args[0]), nil
}

// executeSQL runs one read-only statement against every database of a domain
// and concatenates the rows. For the sharded messages domain a shard that does
// not hold the referenced conversation table is skipped rather than failing the
// whole query.
//
// A merged result is a concatenation, not a globally ranked one: wcctl
// cannot re-rank rows it received as untyped columns, so ORDER BY and LIMIT are
// evaluated per shard. That is reported in the metadata rather than hidden.
func executeSQL(databases []resolvedDatabase, domain databaseDomain, statement string, limit int, explain, dryRun bool) (sqlEnvelope, error) {
	if strings.TrimSpace(statement) == "" {
		return sqlEnvelope{}, fmt.Errorf("empty SQL query")
	}
	if err := validateSQLStatement(statement); err != nil {
		return sqlEnvelope{}, err
	}
	capped := capStatement(statement, limit+1)

	merged := len(databases) > 1
	shards := make([]string, 0, len(databases))
	queries := make([]sqlQuery, 0, len(databases))
	for _, database := range databases {
		shards = append(shards, database.Shard)
		queries = append(queries, sqlQuery{DB: database.Shard, SQL: capped})
	}
	metadata := sqlMetadata{
		Domain:   domain.name,
		Shards:   shards,
		Merged:   merged,
		Ordering: "single-database",
		RowCap:   limit,
	}
	if merged {
		metadata.Ordering = "per-shard"
		metadata.Warnings = append(metadata.Warnings, perShardOrderWarning)
	}
	if explain || dryRun {
		metadata.Queries = queries
	}
	if dryRun {
		return sqlEnvelope{Status: "dry_run", Data: []map[string]any{}, Metadata: metadata}, nil
	}

	rows := make([]map[string]any, 0)
	shardRows := make(map[string]int, len(databases))
	matched := 0
	var lastSkipped error
	for _, database := range databases {
		result, err := querySQLCipher(database.Path, database.AESKey, capped, false)
		if err != nil {
			if domain.sharded && isMissingConversationTable(err) {
				lastSkipped = err
				continue
			}
			return sqlEnvelope{}, fmt.Errorf("query %s: %w", database.Shard, err)
		}
		matched++
		if merged {
			for _, row := range result {
				if _, exists := row["_shard"]; !exists {
					row["_shard"] = database.Shard
				}
			}
		}
		shardRows[database.Shard] = len(result)
		rows = append(rows, result...)
	}
	if domain.sharded && matched == 0 {
		if lastSkipped != nil {
			return sqlEnvelope{}, fmt.Errorf("no message shard holds that table: %w", lastSkipped)
		}
		return sqlEnvelope{}, fmt.Errorf("no message shard holds that table")
	}

	metadata.Truncated = len(rows) > limit
	if metadata.Truncated {
		rows = rows[:limit]
		if merged {
			metadata.Warnings = append(metadata.Warnings,
				"the row cap truncated a per-shard concatenation, so later shards may be under-represented; narrow the query or raise -limit")
		}
	}
	metadata.Returned = len(rows)
	if merged {
		metadata.ShardRows = shardRows
	}

	envelope := sqlEnvelope{Status: "ok", Data: rows, Metadata: metadata}
	switch {
	case metadata.Truncated:
		envelope.Status = "truncated"
	case len(rows) == 0:
		envelope.Status = "empty"
	}
	return envelope, nil
}

// capStatement wraps a query in a common table expression so SQLite enforces
// the row cap, requesting one extra row so truncation is detectable. The CTE
// form keeps the cap attached to the caller's statement as a whole.
func capStatement(statement string, limit int) string {
	trimmed := strings.TrimRight(strings.TrimSpace(statement), "; \t\r\n")
	return fmt.Sprintf("WITH wcctl_query AS (\n%s\n)\nSELECT * FROM wcctl_query LIMIT %d", trimmed, limit)
}

// validateSQLStatement rejects input that would escape the row-cap wrapper:
// unbalanced parentheses, unterminated literals or comments, and anything
// following a statement separator. Only the first statement would ever be
// prepared, so a trailing statement is refused instead of silently discarded.
//
// This is a guard against confusing input, not a security boundary. Read-only
// access is guaranteed by sqlcipher.Query (SQLITE_OPEN_READONLY, query_only
// and the sqlite3_stmt_readonly refusal), which holds regardless of what this
// function accepts.
func validateSQLStatement(statement string) error {
	depth := 0
	separated := false
	for index := 0; index < len(statement); index++ {
		character := statement[index]
		switch character {
		case '\'', '"', '`':
			closing := strings.IndexByte(statement[index+1:], character)
			if closing < 0 {
				return fmt.Errorf("unterminated quoted literal in query")
			}
			index += closing + 1
			continue
		case '-':
			if index+1 < len(statement) && statement[index+1] == '-' {
				end := strings.IndexByte(statement[index:], '\n')
				if end < 0 {
					// The comment runs to the end of the input; stop scanning
					// but still apply the closing checks below.
					index = len(statement)
					continue
				}
				index += end
				continue
			}
		case '/':
			if index+1 < len(statement) && statement[index+1] == '*' {
				end := strings.Index(statement[index+2:], "*/")
				if end < 0 {
					return fmt.Errorf("unterminated block comment in query")
				}
				index += end + 3
				continue
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return fmt.Errorf("unbalanced parentheses in query")
			}
		case ';':
			if depth == 0 {
				separated = true
			}
			continue
		}
		if separated && character > ' ' {
			return fmt.Errorf("only one SQL statement may be executed per invocation")
		}
	}
	if depth != 0 {
		return fmt.Errorf("unbalanced parentheses in query")
	}
	return nil
}

// isMissingConversationTable reports whether an error means a shard simply does
// not hold the requested per-conversation table, which is normal for the
// sharded messages domain. Any other missing table is a real error.
func isMissingConversationTable(err error) bool {
	if err == nil {
		return false
	}
	const marker = "no such table: "
	message := err.Error()
	start := strings.Index(message, marker)
	if start < 0 {
		return false
	}
	name := message[start+len(marker):]
	if end := strings.IndexAny(name, " \t\r\n("); end >= 0 {
		name = name[:end]
	}
	return isMessageTableName(name)
}

func printSQLResult(output io.Writer, envelope sqlEnvelope) error {
	for _, query := range envelope.Metadata.Queries {
		if _, err := fmt.Fprintf(output, "-- %s\n%s\n\n", query.DB, query.SQL); err != nil {
			return err
		}
	}
	if envelope.Status == "dry_run" {
		return nil
	}
	if len(envelope.Data) == 0 {
		if _, err := fmt.Fprintln(output, "(0 rows)"); err != nil {
			return err
		}
		return printSQLWarnings(output, envelope)
	}
	columns := sqlResultColumns(envelope.Data)
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	buffer := bufio.NewWriter(writer)
	header := make([]string, len(columns))
	for i, column := range columns {
		header[i] = strings.ToUpper(column)
	}
	if _, err := fmt.Fprintln(buffer, strings.Join(header, "\t")); err != nil {
		return err
	}
	for _, row := range envelope.Data {
		cells := make([]string, len(columns))
		for i, column := range columns {
			cells[i] = truncateMessage(singleLine(formatSQLValue(row[column])), 200)
		}
		if _, err := fmt.Fprintln(buffer, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	if err := buffer.Flush(); err != nil {
		return err
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	footer := fmt.Sprintf("(%d rows from %s", envelope.Metadata.Returned, strings.Join(envelope.Metadata.Shards, ", "))
	if envelope.Metadata.Truncated {
		footer += fmt.Sprintf("; truncated at %d", envelope.Metadata.RowCap)
	}
	footer += ")"
	if _, err := fmt.Fprintln(output, footer); err != nil {
		return err
	}
	return printSQLWarnings(output, envelope)
}

func printSQLWarnings(output io.Writer, envelope sqlEnvelope) error {
	for _, warning := range envelope.Metadata.Warnings {
		if _, err := fmt.Fprintf(output, "warning: %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}

// sqlResultColumns collects the union of column names across rows, sorted for a
// stable human table. The JSON envelope preserves every field regardless.
func sqlResultColumns(rows []map[string]any) []string {
	seen := make(map[string]bool)
	var columns []string
	for _, row := range rows {
		for key := range row {
			if !seen[key] {
				seen[key] = true
				columns = append(columns, key)
			}
		}
	}
	sort.Strings(columns)
	return columns
}

func formatSQLValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case []byte:
		return fmt.Sprintf("<%d bytes>", len(typed))
	default:
		return fmt.Sprintf("%v", typed)
	}
}
