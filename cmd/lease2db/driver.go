// SPDX-License-Identifier: GPL-3.0-or-later

package main

// The PostgreSQL driver. It understands the usual connection strings and
// URLs, the sslmode/sslcert/sslkey/sslrootcert settings among them.
import _ "github.com/jackc/pgx/v5/stdlib"

const driver = "pgx"
