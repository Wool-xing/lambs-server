package db

import (
	"fmt"
	"strings"
)

// DataSource abstracts read/write access to a managed project's database.
// Each database type (postgres, sqlite, future: mysql, mongodb, redis) implements this interface.
type DataSource interface {
	// ListCollections returns all user-facing collections/tables.
	ListCollections() ([]string, error)
	// ReadItems returns rows, column names and primary key column of a collection.
	ReadItems(collection string) ([]map[string]interface{}, []string, string, error)
	// InsertItem inserts one item. data maps column name -> value.
	InsertItem(collection string, data map[string]interface{}) error
	// UpdateItem updates one item identified by pkCol=pkVal.
	UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error
	// DeleteItem deletes one item identified by pkCol=pkVal.
	DeleteItem(collection, pkCol, pkVal string) error
}

// parseScheme extracts the URL scheme from a DSN, lowercased.
func parseScheme(dsn string) string {
	idx := strings.Index(dsn, "://")
	if idx <= 0 {
		return ""
	}
	return strings.ToLower(dsn[:idx])
}

// NewDataSource builds the adapter matching the DSN's scheme.
func NewDataSource(dsn string) (DataSource, error) {
	if dsn == "" || dsn == "—" {
		return nil, fmt.Errorf("未配置数据源")
	}
	scheme := parseScheme(dsn)
	// Normalize driver suffixes: postgresql+asyncpg → postgresql
	if i := strings.Index(scheme, "+"); i > 0 {
		scheme = scheme[:i]
	}
	switch scheme {
	case "postgres", "postgresql":
		return &PostgresSource{dsn: dsn}, nil
	case "sqlite":
		return &SQLiteSource{dsn: dsn}, nil
	case "mysql":
		return &MySQLSource{dsn: dsn}, nil
	default:
		return nil, fmt.Errorf("不支持的数据源类型: %s", scheme)
	}
}
