// Package db opens SQLite files. The driver is pure Go (modernc.org/sqlite),
// so the app builds into one binary with no C compiler and nothing to install.
package db

import (
	"database/sql"
	"strings"
)

// OpenReadOnly opens an existing SQLite file without ever writing to it.
func OpenReadOnly(path string) (*sql.DB, error) {
	return open(path, "mode=ro&_pragma=busy_timeout(5000)")
}

// OpenReadWrite opens (or creates) a SQLite file.
func OpenReadWrite(path string) (*sql.DB, error) {
	return open(path, "_pragma=busy_timeout(5000)")
}

func open(path, params string) (*sql.DB, error) {
	conn, err := sql.Open(driverName, "file:"+uriPath(path)+"?"+params)
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// uriPath escapes the characters that mean something in a SQLite URI.
func uriPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
}

// QuoteString makes a SQL string literal, for the few statements that can't take parameters.
func QuoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
