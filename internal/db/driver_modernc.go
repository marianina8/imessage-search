//go:build !cgotest

package db

import _ "modernc.org/sqlite" // registers the "sqlite" driver

const driverName = "sqlite"
