# lease2db

`lease2db` follows the lease file of an [ISC DHCP server](https://www.isc.org/dhcp/)
(`dhcpd.leases`) and keeps the history of **who held which address, and
when** in a PostgreSQL database.

ISC dhcpd only knows the present: its lease file says who has an address
now, and forgets everybody who had it before. `lease2db` turns that file
into a history you can ask questions of:

- Who had 192.0.2.57 last Tuesday at 21:40?
- Which addresses has this MAC address had, and on which switch port?
- How full is every network right now?

It is one static binary. It does not talk to dhcpd, does not change its
configuration and does not need to run as root: it only reads two files.

## How it works

dhcpd appends a `lease <ip> { ... }` block to its lease file every time
something happens to a lease, and now and then rewrites the whole file
with one block per lease. `lease2db` reads the blocks as they appear and
keeps **one row for every time a client held an address** - from the
moment it got it until it gave it up, lost it, or somebody else got it:

- a renewal extends the row; it does not make a new one
- a release, an expiry or another client taking the address ends the row
- the same client coming back later starts a new row

Because the lease file repeats itself (after every rewrite, and whenever
`lease2db` starts), every block is compared with what is already stored.
Reading the same file any number of times gives the same rows, so the
program can be stopped, restarted or moved to another machine at any
time. If the database is away, nothing is lost: the file is still there,
and `lease2db` catches up when the database is back.

## Install

Download the archive for your machine from the
[releases](https://github.com/dhtech/lease2db/releases) page and check
it against `SHA256SUMS`:

```sh
tar xzf lease2db_<version>_linux_amd64.tar.gz
install -m 755 lease2db_<version>_linux_amd64/lease2db /usr/local/bin/
lease2db -version
```

The released binaries are built with the latest stable Go. Or build it
yourself (Go 1.25 or newer):

```sh
go build ./cmd/lease2db
```

## The database

`lease2db` creates and updates its own tables. All it needs is a
PostgreSQL database and a role that may create tables in it:

```sql
CREATE ROLE lease2db LOGIN PASSWORD '...';
CREATE DATABASE dhcp OWNER lease2db;
```

The tests run against PostgreSQL 17.

## Run

```sh
lease2db -dsn 'host=db.example.net dbname=dhcp user=lease2db password=... sslmode=verify-full' \
         -leases /var/lib/dhcp/dhcpd.leases \
         -scopes /etc/dhcp/dhcpd.conf
```

| Flag | Default | |
|---|---|---|
| `-dsn` | `$LEASE2DB_DSN` | PostgreSQL connection string or `postgres://` URL |
| `-leases` | `/var/lib/dhcp/dhcpd.leases` | dhcpd's lease file |
| `-scopes` | *(none)* | dhcpd configuration file with the `subnet` declarations |
| `-interval` | `1s` | how often to look for new leases |
| `-metrics` | *(none)* | write Prometheus metrics to this file |
| `-version` | | print the version and exit |

Run it on the machine where dhcpd runs, as any user that can read the
two files. An example systemd unit is in [`contrib/`](contrib/lease2db.service).
Prefer `LEASE2DB_DSN` in an environment file over `-dsn` when the
connection string holds a password: command lines are visible to every
user of the machine.

### Connecting without a password

The connection string takes the usual `sslmode`, `sslrootcert`, `sslcert`
and `sslkey` settings, so `lease2db` can log in with a client certificate
instead of a password:

```
host=db.example.net dbname=dhcp user=lease2db sslmode=verify-full
sslrootcert=/etc/ssl/ca.pem sslcert=/etc/ssl/dhcp1.pem sslkey=/etc/ssl/private/dhcp1.key
```

with a line like this in the server's `pg_hba.conf` (and `ssl_ca_file`
set):

```
hostssl dhcp lease2db 192.0.2.10/32 cert map=lease2db
```

### Scopes

With `-scopes`, the networks dhcpd hands addresses out in - every
`subnet` declaration with its `range`s and `option routers` - are kept in
the table `scopes`, and reread whenever the file changes. That is what
makes "how full is this network" answerable. `include` statements are not
followed: point `-scopes` at the file that holds the subnets.

A network gets a name and a VLAN number when the line before its
declaration says so, in a comment of this form:

```
#@ NET OFFICE 192.0.2.0/24 - VLAN 10
subnet 192.0.2.0 netmask 255.255.255.0 {
	range 192.0.2.20 192.0.2.250;
	option routers 192.0.2.1;
}
```

Without `-scopes` the lease history works just the same; only the
per-network views stay empty.

## What is in the database

**`leases`** - one row for every time a client held an address.

| Column | |
|---|---|
| `ip`, `mac` | the address and the client (`mac` is NULL for an abandoned address) |
| `first_seen` | when this client got the address |
| `starts` | its latest renewal |
| `ends` | when the lease runs out, or ended; NULL = never |
| `state` | dhcpd's binding state: `active`, `free`, `expired`, `released`, `abandoned`, ... |
| `current` | true for the latest row of its address |
| `hostname` | the name the client sent |
| `circuit_id`, `remote_id` | relay agent information (option 82): where the client is plugged in |
| `vendor_class` | the client's vendor class, when dhcpd is set up to record it |
| `updated` | when the row last changed |

**`scopes`** - the networks: `network`, `name`, `vlan`, `gateway`,
`addresses` (in the dynamic ranges), `active` (in the configuration now).

**`leases_active`** (view) - who holds which address right now, with the
network's name and VLAN.

**`scopes_usage`** (view) - for every active network, its number of
addresses and how many are leased right now.

### Questions to ask it

Who had this address at that time?

```sql
SELECT mac, hostname, circuit_id, first_seen, ends
FROM leases
WHERE ip = '192.0.2.57'
  AND first_seen <= '2026-01-13 21:40+01'
  AND (ends IS NULL OR ends >= '2026-01-13 21:40+01');
```

Where has this client been?

```sql
SELECT ip, first_seen, ends, state, circuit_id
FROM leases WHERE mac = '00:00:5e:00:53:01' ORDER BY first_seen;
```

How full is every network?

```sql
SELECT name, vlan, leases, addresses,
       round(100.0 * leases / nullif(addresses, 0)) AS percent
FROM scopes_usage ORDER BY percent DESC NULLS LAST;
```

The views work directly as data sources for Grafana's PostgreSQL plugin.

### Good to know

- Times in the lease file are UTC; the columns are `timestamptz`.
- A lease handed out and gone again while `lease2db` was not running is
  only in the history if dhcpd had not yet rewritten its file - dhcpd
  keeps the last word about every address, not everything in between.
- Addresses given by `fixed-address` are not in the lease file at all, so
  they are not in the history either.
- Old rows are never deleted. Delete what you no longer want yourself,
  for example `DELETE FROM leases WHERE NOT current AND ends < now() - interval '1 year'`.
- DHCPv6 (`dhcpd6.leases`) is not read.

## Metrics

With `-metrics /var/lib/prometheus/node-exporter/lease2db.prom` the
program writes its own state for the node exporter's textfile collector
every 15 seconds:

| Metric | |
|---|---|
| `lease2db_connected` | 1 = connected to the database |
| `lease2db_last_success_timestamp_seconds` | when the file was last read and the database written |
| `lease2db_blocks_total` | lease blocks read |
| `lease2db_changes_total` | blocks that changed a row |
| `lease2db_blocks_refused_total` | blocks that could not be understood (they are logged and skipped) |
| `lease2db_errors_total` | times the database connection was lost |
| `lease2db_file_restarts_total` | times the file was read from the top |
| `lease2db_addresses` | addresses with a row in the history |

A sensible alert: `time() - lease2db_last_success_timestamp_seconds > 300`.

## Development

```sh
go test ./...
```

The tests of the database layer need a real PostgreSQL and are skipped
without one. To run them, point `LEASE2DB_TEST_DSN` at a database the
tests may fill and empty:

```sh
LEASE2DB_TEST_DSN='postgres://postgres:test@localhost/lease2db_test?sslmode=disable' go test -p 1 ./...
```

A release is a tag: pushing `vX.Y.Z` builds the archives and publishes
them.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
