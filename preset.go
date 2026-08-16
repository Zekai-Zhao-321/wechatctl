package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// builtinPreset is a named, read-only query the sql command can run in place of
// a raw statement. Each preset targets one domain and reveals the SQL it builds
// through -explain / -dry-run, so it doubles as a worked example.
type builtinPreset struct {
	domain      string
	usage       string
	description string
	build       func(databases []resolvedDatabase, args []string) (string, error)
}

var builtinPresets = map[string]builtinPreset{
	"from-sender": {
		domain:      "fts",
		usage:       "from-sender <wxid>",
		description: "every text message a wxid sent, across all conversations (via the FTS shadow)",
		build:       buildFromSenderPreset,
	},
}

// buildPreset resolves a preset, then builds its statement against that preset's
// domain databases.
func buildPreset(user storedUser, name string, args []string) (databaseDomain, string, error) {
	preset, ok := builtinPresets[name]
	if !ok {
		return databaseDomain{}, "", fmt.Errorf("unknown preset %q; run 'wcctl sql -list-presets'", name)
	}
	domain, err := resolveDomain(preset.domain)
	if err != nil {
		return databaseDomain{}, "", err
	}
	databases, err := domainDatabases(user, domain)
	if err != nil {
		return databaseDomain{}, "", err
	}
	statement, err := preset.build(databases, args)
	if err != nil {
		return databaseDomain{}, "", err
	}
	return domain, statement, nil
}

// buildFromSenderPreset returns every text message a wxid sent, in any
// conversation, by scanning the FTS shadow content tables. c5 is a rowid into
// message_fts.db's global name2id, so the wxid is resolved through it once.
func buildFromSenderPreset(databases []resolvedDatabase, args []string) (string, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("preset from-sender requires exactly one wxid argument")
	}
	if len(databases) == 0 {
		return "", fmt.Errorf("no fts database available")
	}
	tables, err := listFTSContentTables(databases[0])
	if err != nil {
		return "", err
	}
	if len(tables) == 0 {
		return "", fmt.Errorf("no FTS content tables found in %s", databases[0].Shard)
	}
	selects := make([]string, 0, len(tables))
	for _, table := range tables {
		selects = append(selects, "SELECT c0, c4, c5, c6 FROM "+quoteSQLIdentifier(table))
	}
	// c4 and c5 are rowids into message_fts.db's single global name2id table, so
	// both resolve to usernames without crossing a per-shard identity space.
	statement := fmt.Sprintf(`SELECT
  (SELECT username FROM name2id WHERE rowid = c4) AS conversation,
  c6 AS create_time,
  c0 AS text
FROM (
%s
)
WHERE c5 = (SELECT rowid FROM name2id WHERE username = %s)
ORDER BY c6`, strings.Join(selects, "\nUNION ALL "), quoteSQLString(strings.TrimSpace(args[0])))
	return statement, nil
}

func listFTSContentTables(database resolvedDatabase) ([]string, error) {
	var rows []struct {
		Name string `json:"name"`
	}
	statement := `SELECT name FROM sqlite_master
WHERE type='table' AND name LIKE 'message\_fts\_v4\_%\_content' ESCAPE '\'
ORDER BY name;`
	if err := queryDatabaseJSON(database.Path, database.AESKey, statement, &rows); err != nil {
		return nil, fmt.Errorf("list FTS content tables in %s: %w", database.Shard, err)
	}
	tables := make([]string, 0, len(rows))
	for _, row := range rows {
		tables = append(tables, row.Name)
	}
	return tables, nil
}

func printPresetList(output io.Writer, jsonOutput bool) error {
	type presetInfo struct {
		Name        string `json:"name"`
		Domain      string `json:"domain"`
		Usage       string `json:"usage"`
		Description string `json:"description"`
	}
	names := make([]string, 0, len(builtinPresets))
	for name := range builtinPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	infos := make([]presetInfo, 0, len(names))
	for _, name := range names {
		preset := builtinPresets[name]
		infos = append(infos, presetInfo{Name: name, Domain: preset.domain, Usage: preset.usage, Description: preset.description})
	}
	if jsonOutput {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(infos)
	}
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "NAME\tDOMAIN\tUSAGE\tDESCRIPTION"); err != nil {
		return err
	}
	for _, info := range infos {
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", info.Name, info.Domain, info.Usage, info.Description); err != nil {
			return err
		}
	}
	return writer.Flush()
}
