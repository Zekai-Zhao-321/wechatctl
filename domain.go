package main

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// databaseDomain names a logical WeChat database that the sql and schema
// commands target with -db. Each domain maps to one stored SQLCipher file,
// except messages, which is sharded across the message_N.db files.
type databaseDomain struct {
	name      string
	aliases   []string
	directory string
	basename  string
	sharded   bool
}

// databaseDomains lists every -db value. The first alias of each domain is the
// natural Chinese term a WeChat user reaches for; the canonical name is English.
var databaseDomains = []databaseDomain{
	{name: "messages", aliases: []string{"消息", "聊天记录"}, directory: "message", sharded: true},
	{name: "contact", aliases: []string{"通讯录", "联系人"}, directory: "contact", basename: "contact.db"},
	{name: "session", aliases: []string{"会话", "最近聊天"}, directory: "session", basename: "session.db"},
	{name: "sns", aliases: []string{"朋友圈"}, directory: "sns", basename: "sns.db"},
	{name: "fts", aliases: []string{"搜索"}, directory: "message", basename: "message_fts.db"},
	{name: "favorite", aliases: []string{"收藏"}, directory: "favorite", basename: "favorite.db"},
	{name: "biz", aliases: []string{"公众号", "订阅号", "服务号"}, directory: "message", basename: "biz_message_0.db"},
}

// resolveDomain maps a -db value (the canonical English name or a Chinese
// alias) to its domain.
func resolveDomain(value string) (databaseDomain, error) {
	trimmed := strings.TrimSpace(value)
	lowered := strings.ToLower(trimmed)
	for _, domain := range databaseDomains {
		if domain.name == lowered {
			return domain, nil
		}
		for _, alias := range domain.aliases {
			if alias == trimmed {
				return domain, nil
			}
		}
	}
	return databaseDomain{}, fmt.Errorf("unknown -db %q; valid: %s", value, domainList())
}

// domainList renders the valid -db values with their leading Chinese alias, for
// use in usage and error messages.
func domainList() string {
	labels := make([]string, 0, len(databaseDomains))
	for _, domain := range databaseDomains {
		if len(domain.aliases) > 0 {
			labels = append(labels, fmt.Sprintf("%s(%s)", domain.name, domain.aliases[0]))
			continue
		}
		labels = append(labels, domain.name)
	}
	return strings.Join(labels, " ")
}

// storedPaths returns the key-store paths that belong to this domain, sorted.
func (domain databaseDomain) storedPaths(user storedUser) []string {
	if domain.sharded {
		return storedMessageDatabases(user)
	}
	var paths []string
	for path := range user.Databases {
		if filepath.Base(filepath.Dir(path)) == domain.directory && filepath.Base(path) == domain.basename {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// resolvedDatabase pairs a stored database path with its decoded 32-byte key.
type resolvedDatabase struct {
	Path   string
	AESKey []byte
	Shard  string
}

// domainDatabases resolves every stored database for a domain and decodes its
// key, returning an error if the domain has no entry or an unusable key.
func domainDatabases(user storedUser, domain databaseDomain) ([]resolvedDatabase, error) {
	paths := domain.storedPaths(user)
	if len(paths) == 0 {
		if domain.sharded {
			return nil, fmt.Errorf("no %s/message_N.db entries in key store", domain.directory)
		}
		return nil, fmt.Errorf("no %s/%s entry in key store", domain.directory, domain.basename)
	}
	databases := make([]resolvedDatabase, 0, len(paths))
	for _, path := range paths {
		stored := user.Databases[path]
		aesKey, err := hex.DecodeString(stored.AESKey)
		if err != nil || len(aesKey) != 32 {
			return nil, fmt.Errorf("%s has an invalid AES-256 key", filepath.Base(path))
		}
		databases = append(databases, resolvedDatabase{Path: path, AESKey: aesKey, Shard: filepath.Base(path)})
	}
	return databases, nil
}

// quoteSQLIdentifier wraps a table or column name as a SQLite identifier.
func quoteSQLIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// quoteSQLString wraps a value as a single-quoted SQLite string literal.
func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
