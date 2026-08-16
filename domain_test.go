package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestResolveDomainAcceptsNameAndAlias(t *testing.T) {
	cases := map[string]string{
		"messages": "messages",
		"消息":       "messages",
		"通讯录":      "contact",
		"朋友圈":      "sns",
		"SNS":      "sns",
		"公众号":      "biz",
	}
	for input, want := range cases {
		domain, err := resolveDomain(input)
		if err != nil {
			t.Fatalf("resolveDomain(%q) error: %v", input, err)
		}
		if domain.name != want {
			t.Fatalf("resolveDomain(%q) = %q, want %q", input, domain.name, want)
		}
	}
	if _, err := resolveDomain("nope"); err == nil || !strings.Contains(err.Error(), "unknown -db") {
		t.Fatalf("expected unknown -db error, got %v", err)
	}
}

func TestDomainStoredPathsSelectByLayout(t *testing.T) {
	user := storedUser{Databases: map[string]storedDatabase{
		"/db/message/message_0.db":     {},
		"/db/message/message_2.db":     {},
		"/db/message/message_fts.db":   {},
		"/db/message/biz_message_0.db": {},
		"/db/contact/contact.db":       {},
		"/db/sns/sns.db":               {},
	}}
	cases := map[string][]string{
		"messages": {"/db/message/message_0.db", "/db/message/message_2.db"},
		"fts":      {"/db/message/message_fts.db"},
		"biz":      {"/db/message/biz_message_0.db"},
		"contact":  {"/db/contact/contact.db"},
		"sns":      {"/db/sns/sns.db"},
	}
	for name, want := range cases {
		domain, err := resolveDomain(name)
		if err != nil {
			t.Fatal(err)
		}
		got := domain.storedPaths(user)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s paths = %v, want %v", name, got, want)
		}
	}
}

func TestDomainDatabasesRejectsBadKey(t *testing.T) {
	contact, err := resolveDomain("contact")
	if err != nil {
		t.Fatal(err)
	}
	good := storedUser{Databases: map[string]storedDatabase{
		"/db/contact/contact.db": {AESKey: hex.EncodeToString(make([]byte, 32))},
	}}
	databases, err := domainDatabases(good, contact)
	if err != nil || len(databases) != 1 || len(databases[0].AESKey) != 32 {
		t.Fatalf("domainDatabases good = %#v, err %v", databases, err)
	}
	bad := storedUser{Databases: map[string]storedDatabase{
		"/db/contact/contact.db": {AESKey: "abcd"},
	}}
	if _, err := domainDatabases(bad, contact); err == nil || !strings.Contains(err.Error(), "invalid AES-256 key") {
		t.Fatalf("expected invalid key error, got %v", err)
	}
	empty := storedUser{Databases: map[string]storedDatabase{}}
	if _, err := domainDatabases(empty, contact); err == nil || !strings.Contains(err.Error(), "no contact/contact.db entry") {
		t.Fatalf("expected missing entry error, got %v", err)
	}
}
