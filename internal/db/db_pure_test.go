package db

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestIsNumericAndSQLiteVal(t *testing.T) {
	if !isNumeric("123") || isNumeric("1.5") || isNumeric("-2") || isNumeric("1a") || isNumeric("") {
		t.Error("isNumeric misbehaving — digits only")
	}
	if sqliteVal("abc") != "'abc'" {
		t.Errorf("sqliteVal(abc) = %q", sqliteVal("abc"))
	}
	if sqliteVal("it's") != "'it''s'" {
		t.Errorf("sqliteVal quote escape = %q", sqliteVal("it's"))
	}
	if sqliteVal("123") != "123" {
		t.Errorf("sqliteVal numeric = %q", sqliteVal("123"))
	}
}

func TestDocToRowAndPkFilter(t *testing.T) {
	oid := primitive.NewObjectID()
	doc := bson.M{"_id": oid, "name": "n"}
	row := docToRow(doc)
	if row["_id"] != oid.Hex() {
		t.Errorf("docToRow ObjectID not hexed: %v", row["_id"])
	}
	if row["name"] != "n" {
		t.Errorf("docToRow = %v", row)
	}
	if got := pkFilter("_id", "abc"); got["_id"] != "abc" {
		t.Errorf("pkFilter = %v", got)
	}
}

func TestMSSQLHelpers(t *testing.T) {
	if got := mssqlPlaceholders(3); got != "@p1,@p2,@p3" {
		t.Errorf("mssqlPlaceholders(3) = %q", got)
	}
	if got := mssqlQuoteIdent("order"); got != "[order]" {
		t.Errorf("mssqlQuoteIdent = %q", got)
	}
	// The identifier is charset-validated upstream (validateTable); quoting
	// is a plain bracket wrap.
	if got := mssqlQuoteIdent("a]b"); got != "[a]b]" {
		t.Errorf("mssqlQuoteIdent = %q", got)
	}
	sql := mssqlSelectSQL("users", 20, 40)
	if !strings.Contains(sql, "OFFSET 40 ROWS") || !strings.Contains(sql, "FETCH NEXT 20 ROWS ONLY") {
		t.Errorf("mssqlSelectSQL = %q", sql)
	}
}

func TestStatsHelpers(t *testing.T) {
	if typeKind("PostgreSQL") != "sql" || typeKind("SQL Server") != "sql" || typeKind("MSSQL") != "sql" ||
		typeKind("MongoDB") != "mongo" || typeKind("Redis") != "redis" || typeKind("REST") != "rest" ||
		typeKind("Qdrant") != "" {
		t.Error("typeKind misbehaving")
	}
	if toInt(int64(42)) != 42 || toInt(3.9) != 3 || toInt("abc") != 0 || toInt(nil) != 0 {
		t.Error("toInt misbehaving")
	}
	if toFloat(1.5) != 1.5 || toFloat("x") != 0 || toFloat(2) != 2.0 {
		t.Error("toFloat misbehaving")
	}
	if redisInfoInt("used_memory:1024\r\n", "used_memory") != 1024 || redisInfoInt("", "x") != 0 {
		t.Error("redisInfoInt misbehaving")
	}
	if redisInfoStr("redis_version:7.0.0\r\n", "redis_version") != "7.0.0" {
		t.Error("redisInfoStr misbehaving")
	}
}

func TestValidateKeyAndStr(t *testing.T) {
	if validateKey("good:key") != nil || validateKey("bad key") == nil || validateKey("") == nil {
		t.Error("validateKey misbehaving")
	}
	if str(nil) != "" || str(42) != "42" || str("x") != "x" {
		t.Error("str misbehaving")
	}
}

func TestNewUUIDv4(t *testing.T) {
	a, b := newUUIDv4(), newUUIDv4()
	if len(a) != 36 || a == b {
		t.Errorf("newUUIDv4 = %q %q", a, b)
	}
}
