package install

import (
	"os"
	"strings"
	"testing"
)

func TestEmbeddedSchemaMatchesFile(t *testing.T) {
	raw, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile("../../sql/001_schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(disk) {
		t.Fatal("embedded schema drifted from sql/001_schema.sql")
	}
}

func TestSplitSQLKeepsCreate(t *testing.T) {
	parts := splitSQL("-- comment\nCREATE TABLE demo (id int);\n\nCREATE INDEX idx ON demo (id);\n")
	if len(parts) != 2 {
		t.Fatalf("parts %d", len(parts))
	}
}

func TestDSNRejectsOddNames(t *testing.T) {
	_, err := (Form{Host: "127.0.0.1", Port: "5432", Database: "bad-name", User: "dufaka"}).dsn("postgres")
	if err == nil {
		t.Fatal("expected reject")
	}
}

func TestDSNEncodesPassword(t *testing.T) {
	got, err := (Form{Host: "127.0.0.1", Port: "5432", Database: "dufaka", User: "dufaka", Password: "a b"}).dsn("dufaka")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "a b") || !strings.Contains(got, "dufaka") {
		t.Fatal(got)
	}
}

// Round 2 B-7: a new install has the order query password switched on.
func TestNewInstallEnablesSearchPassword(t *testing.T) {
	got := map[string]string{}
	for _, kv := range siteSettings(Form{Title: "Shop", AppURL: "https://shop.example/"}) {
		got[kv[0]] = kv[1]
	}
	if got["is_open_search_pwd"] != "1" {
		t.Fatalf("is_open_search_pwd=%q, want 1", got["is_open_search_pwd"])
	}
	if got["title"] != "Shop" || got["app_url"] != "https://shop.example" || got["order_expire_time"] != "5" {
		t.Fatalf("seed settings changed unexpectedly: %v", got)
	}
}
