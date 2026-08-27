package db

import (
	"strings"
	"testing"
)

// TestMSSQLGoDSN pins the mssql:// → sqlserver:// driver DSN conversion.
// Defaults: encrypt=true + trustservercertificate=true (user decision 2026-08-19:
// default encryption ON, opt-out via query params — mirrors go-mssqldb modern
// defaults so TLS is never silently dropped).
func TestMSSQLGoDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "basic with database",
			dsn:  "mssql://sa:Passw0rd@127.0.0.1:1433/mydb",
			want: "sqlserver://sa:Passw0rd@127.0.0.1:1433?database=mydb&dial+timeout=10&encrypt=true&trustservercertificate=true",
		},
		{
			name: "encrypt opt-out via query",
			dsn:  "mssql://sa:p@h/db?encrypt=disable",
			want: "sqlserver://sa:p@h?database=db&dial+timeout=10&encrypt=disable",
		},
		{
			name: "trustservercertificate opt-out",
			dsn:  "mssql://sa:p@h/db?trustservercertificate=false",
			want: "sqlserver://sa:p@h?database=db&dial+timeout=10&encrypt=true&trustservercertificate=false",
		},
		{
			name: "special chars in password survive url roundtrip",
			dsn:  "mssql://sa:p%40ss%3Aw@h/db",
			want: "sqlserver://sa:p%40ss%3Aw@h?database=db&dial+timeout=10&encrypt=true&trustservercertificate=true",
		},
		{
			name: "no path still carries an empty database param",
			dsn:  "mssql://sa:pw@h",
			want: "sqlserver://sa:pw@h?database=&dial+timeout=10&encrypt=true&trustservercertificate=true",
		},
		{
			name: "passwordless user",
			dsn:  "mssql://sa@h/db",
			want: "sqlserver://sa@h?database=db&dial+timeout=10&encrypt=true&trustservercertificate=true",
		},
		{
			name: "dial timeout 0 is forced to the 10s default",
			dsn:  "mssql://sa:pw@h/db?dial%20timeout=0",
			want: "sqlserver://sa:pw@h?database=db&dial+timeout=10&encrypt=true&trustservercertificate=true",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &MSSQLSource{dsn: c.dsn}
			got, err := s.goDSN()
			if err != nil {
				t.Fatalf("goDSN(%q) error: %v", c.dsn, err)
			}
			if got != c.want {
				t.Errorf("goDSN(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}

// TestMSSQLPlaceholders pins @pN positional placeholders — go-mssqldb does
// NOT accept MySQL-style "?" placeholders (copy-paste trap).
func TestMSSQLPlaceholders(t *testing.T) {
	if got := mssqlPlaceholders(3); got != "@p1,@p2,@p3" {
		t.Errorf("mssqlPlaceholders(3) = %q, want @p1,@p2,@p3", got)
	}
	if got := mssqlPlaceholders(0); got != "" {
		t.Errorf("mssqlPlaceholders(0) = %q, want empty", got)
	}
}

// TestMSSQLIdentifiers pins [bracket] quoting — SQL Server uses square
// brackets, not backticks (another MySQL copy-paste trap).
func TestMSSQLIdentifiers(t *testing.T) {
	if got := mssqlQuoteIdent("Users"); got != "[Users]" {
		t.Errorf("mssqlQuoteIdent(Users) = %q, want [Users]", got)
	}
}

// TestMSSQLReadItemsSQL pins OFFSET/FETCH pagination (version floor 2012+,
// user decision) and bracketed identifiers.
func TestMSSQLReadItemsSQL(t *testing.T) {
	sql := mssqlSelectSQL("Orders", 20, 40)
	if !strings.Contains(sql, "FROM [Orders]") {
		t.Errorf("mssqlSelectSQL missing bracketed table: %s", sql)
	}
	if !strings.Contains(sql, "OFFSET 40 ROWS FETCH NEXT 20 ROWS ONLY") {
		t.Errorf("mssqlSelectSQL missing OFFSET/FETCH: %s", sql)
	}
	// limit=0 → default cap 500, no OFFSET clause
	sql = mssqlSelectSQL("Orders", 0, 0)
	if !strings.Contains(sql, "FETCH NEXT 500 ROWS ONLY") {
		t.Errorf("mssqlSelectSQL default cap missing: %s", sql)
	}
}

// TestMSSQLPkColumnSQL pins INFORMATION_SCHEMA-based pk discovery.
func TestMSSQLPkColumnSQL(t *testing.T) {
	sql := mssqlPkColumnSQL()
	for _, frag := range []string{"INFORMATION_SCHEMA.KEY_COLUMN_USAGE", "OBJECTPROPERTY", "IsPrimaryKey"} {
		if !strings.Contains(sql, frag) {
			t.Errorf("mssqlPkColumnSQL missing %q: %s", frag, sql)
		}
	}
}
