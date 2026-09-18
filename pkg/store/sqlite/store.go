package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/trisacrypto/envoy/pkg/audit"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store/dsn"
	"github.com/trisacrypto/envoy/pkg/store/errors"
	"github.com/trisacrypto/envoy/pkg/store/models"
	"github.com/trisacrypto/envoy/pkg/store/txn"

	_ "github.com/mattn/go-sqlite3"
	"github.com/rs/zerolog/log"
)

// Connection parameters applied to every sqlite3 connection unless the operator
// overrides them in the DSN query string.
//
// These are the defaults a replicated deployment needs: the hosted image ships the
// database to object storage with Litestream, which requires WAL mode and takes short
// locks of its own while checkpointing. Without a busy timeout those locks surface to
// Envoy as "database is locked" errors rather than a brief wait. Synchronous NORMAL is
// the standard pairing for WAL (a crash can lose the last transactions, not the
// database) and is what go-sqlite3 selects for WAL anyway. Foreign keys are set as a
// connection parameter so that every connection database/sql opens into the pool has
// them on; the one-off PRAGMA below only applies to whichever connection ran it.
var sqliteDefaults = map[string]string{
	"_journal_mode": "WAL",
	"_busy_timeout": "5000",
	"_foreign_keys": "on",
	"_synchronous":  "NORMAL",
}

// txlockParam is the go-sqlite3 connection parameter that selects the statement the
// driver issues to open a transaction: "deferred" (the driver default) sends BEGIN,
// "immediate" sends BEGIN IMMEDIATE. It is deliberately not in sqliteDefaults because
// the two pools below want different values for it.
//
// NOTE: go-sqlite3 has short aliases for several of the parameters above, but not for
// this one: SQLiteDriver.Open reads it as exactly "_txlock" and rejects any value other
// than immediate, exclusive or deferred, so an operator override only has to be
// recognized under that name.
const txlockParam = "_txlock"

// Store implements the store.Store interface using SQLite3 as the storage backend.
//
// The store keeps two connection pools over the same database file, differing only in
// the transaction statement the driver issues. Read transactions begin on the read pool
// with a plain BEGIN; write transactions begin on the write pool, whose DSN carries
// _txlock=immediate, so they take the write lock at BEGIN.
//
// The split exists because a deferred write transaction that reads before it writes has
// to upgrade a shared lock into a write lock, and in WAL mode SQLite fails that upgrade
// with SQLITE_BUSY_SNAPSHOT the moment another connection has committed since the read
// began. That error does not invoke the busy handler at all: the transaction's snapshot
// is already stale, so no amount of waiting could let it proceed, and _busy_timeout
// never applies. The caller just sees "database is locked". BEGIN IMMEDIATE takes the
// write lock up front instead, which is a plain lock contention that the busy handler
// does wait out. Setting _txlock=immediate on a single shared pool would fix the writes
// but serialize every read behind the write lock, which is the concurrency WAL exists to
// provide, hence two pools rather than one parameter.
//
// A readonly store (mode=ro) has no write pool; write is nil there.
type Store struct {
	readonly bool
	read     *sql.DB
	write    *sql.DB
	mkta     models.TravelAddressFactory
}

// Tx implements the store.Tx interface using SQLite3 as the storage backend.
type Tx struct {
	tx   *sql.Tx
	opts *sql.TxOptions
	mkta models.TravelAddressFactory

	// Compliance audit log actor metadata
	actorID   []byte
	actorType enum.Actor
}

//===========================================================================
// Store methods
//===========================================================================

func Open(uri *dsn.DSN) (_ *Store, err error) {
	// Ensure that only SQLite3 connections can be opened.
	if uri.Scheme != dsn.SQLite && uri.Scheme != dsn.SQLite3 {
		return nil, errors.ErrUnknownScheme
	}

	// Require a path in order to open the database connection (no in-memory databases)
	if uri.Path == "" {
		return nil, errors.ErrPathRequired
	}

	// Check if the database file exists, if it doesn't exist it will be created and
	// all migrations will be applied to the database. Otherwise the code will attempt
	// to only apply migrations that have not yet been applied.
	empty := false
	if _, err := os.Stat(uri.Path); os.IsNotExist(err) {
		empty = true
	}

	// Connect to the database using a file: URI so that the connection parameters are
	// honored by the driver (they are dropped if only the path is supplied).
	readParams := connectionParams(uri, false)

	// A readonly store never opens the write pool, so it has no write parameters to
	// render or log.
	writeParams := ""
	if !uri.ReadOnly {
		writeParams = connectionParams(uri, true)
	}

	log.Debug().
		Str("path", uri.Path).
		Str("read_params", readParams).
		Str("write_params", writeParams).
		Bool("readonly", uri.ReadOnly).
		Msg("opening sqlite3 database")

	s := &Store{readonly: uri.ReadOnly}
	if s.read, err = sql.Open("sqlite3", fileURI(uri.Path, readParams)); err != nil {
		return nil, err
	}

	// Ping the database to establish the connection
	if err = s.read.Ping(); err != nil {
		return nil, err
	}

	// The write pool only exists on a writable store: mode=ro cannot serve a write
	// transaction, and BeginTx refuses to open one against a readonly store anyway.
	if !uri.ReadOnly {
		if s.write, err = sql.Open("sqlite3", fileURI(uri.Path, writeParams)); err != nil {
			return nil, err
		}

		if err = s.write.Ping(); err != nil {
			return nil, err
		}
	}

	// Belt and braces: _foreign_keys in the connection string already applies to every
	// pooled connection, but leave the PRAGMA in place so the behavior does not depend
	// on the driver's parameter handling. It is per-connection state, so it has to be
	// run against each pool rather than only the one that happens to be the writer.
	for _, conn := range []*sql.DB{s.read, s.write} {
		if conn == nil {
			continue
		}

		if _, err = conn.Exec("PRAGMA foreign_keys = on"); err != nil {
			return nil, fmt.Errorf("could not enable foreign key support: %w", err)
		}
	}

	// Ensure the schema is initialized
	if err = s.InitializeSchema(empty); err != nil {
		return nil, err
	}

	return s, nil
}

// writer returns the pool that writes outside of Store.BeginTx go through: the write
// pool on a writable store, and the read only pool on a readonly one, where the driver
// refuses the write regardless of which connection carries it.
func (s *Store) writer() *sql.DB {
	if s.write != nil {
		return s.write
	}

	return s.read
}

// connectionParams renders the encoded query string for the sqlite3 connection: the
// replication-safe defaults above, overlaid by whatever the operator put in the DSN
// (compared case-insensitively so that _JOURNAL_MODE overrides _journal_mode), plus
// mode=ro when the DSN asked for a read only database. The write pool additionally gets
// _txlock=immediate; see the Store documentation for why the two pools differ.
//
// NOTE: go-sqlite3 also accepts short aliases (_journal, _timeout, _fk, _sync) and
// prefers them over the long form, so an alias in the DSN overrides a default here too.
func connectionParams(uri *dsn.DSN, write bool) string {
	params := make(url.Values, len(sqliteDefaults)+len(uri.Options)+2)
	for key, value := range sqliteDefaults {
		params.Set(key, value)
	}

	txlock := false
	for key, value := range uri.Options {
		// Canonicalize any option this package sets or inspects: the driver looks its
		// parameters up case-sensitively, so a differently cased key would otherwise be
		// ignored and the default would silently win.
		lower := strings.ToLower(key)
		if _, ok := sqliteDefaults[lower]; ok || lower == txlockParam {
			key = lower
		}

		if key == txlockParam {
			txlock = true
		}

		params.Set(key, value)
	}

	// An operator who put _txlock in the DSN has chosen the locking behavior for the
	// whole store, so it is passed through to both pools untouched and this store keeps
	// behaving exactly as it did before the pools were split; _txlock=deferred is the
	// escape hatch back to the old, single-behavior store. Otherwise only the write pool
	// is switched to BEGIN IMMEDIATE, never the read pool.
	if write && !txlock {
		params.Set(txlockParam, "immediate")
	}

	// The read only flag is explicit in the DSN, so it wins over any mode option. A
	// read only connection also keeps whatever journal mode the file already has:
	// switching an existing rollback-journal database to WAL is a write, and the
	// driver runs that PRAGMA while initializing the connection, so the default would
	// make every read only open of a pre-WAL database fail. An explicit journal mode
	// in the DSN is still passed through.
	if uri.ReadOnly {
		params.Set("mode", "ro")

		if _, explicit := uri.Options["_journal_mode"]; !explicit {
			if _, explicit = uri.Options["_JOURNAL_MODE"]; !explicit {
				params.Del("_journal_mode")
			}
		}
	}

	return params.Encode()
}

// fileURI renders a sqlite file: URI for the given filesystem path and encoded query
// parameters. dsn.Parse has already URL-decoded the path, so it has to be escaped again
// here: a literal "?" or "#" in a filename would otherwise be read by the driver as the
// start of the query or fragment and open a different file than the one os.Stat checked.
func fileURI(path, params string) string {
	escaped := (&url.URL{Path: path}).EscapedPath()

	return "file:" + escaped + "?" + params
}

// Checkpoint flushes the write-ahead log of the database at path into the main file
// and truncates it. Tools that copy or rename the database file on its own (such as
// the remigrate command) must call this first: with WAL journaling, committed
// transactions can otherwise sit in the -wal sidecar and be missing from the copy.
func Checkpoint(path string) (err error) {
	var conn *sql.DB

	if conn, err = sql.Open("sqlite3", fileURI(path, "_busy_timeout=5000")); err != nil {
		return err
	}

	defer conn.Close()

	// The pragma reports rather than errors when it could not finish: busy is 1 if
	// another connection blocked it, and log/checkpointed are the WAL frame counts
	// (-1 when the database is not in WAL mode at all). Anything short of a complete
	// checkpoint means committed transactions are still only in the -wal file, which
	// is exactly what a caller about to copy the main file must not accept.
	var busy, logged, checkpointed int

	if err = conn.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logged, &checkpointed); err != nil {
		return fmt.Errorf("could not checkpoint write-ahead log: %w", err)
	}

	if busy != 0 {
		return fmt.Errorf("could not checkpoint write-ahead log: another connection holds the database")
	}

	if logged != -1 && checkpointed != logged {
		return fmt.Errorf("could not checkpoint write-ahead log: %d of %d frames checkpointed", checkpointed, logged)
	}

	return nil
}

// Close both pools, returning the first error but always attempting both so that a
// failure on one does not leak the connections of the other.
func (s *Store) Close() (err error) {
	if s.write != nil {
		err = s.write.Close()
	}

	if cerr := s.read.Close(); err == nil {
		err = cerr
	}

	return err
}

func (s *Store) Begin(ctx context.Context, opts *sql.TxOptions) (txn.Txn, error) {
	return s.BeginTx(ctx, opts)
}

func (s *Store) BeginTx(ctx context.Context, opts *sql.TxOptions) (_ *Tx, err error) {
	// Ensure the options respect the read-only option specified by the user.
	if opts == nil {
		opts = &sql.TxOptions{ReadOnly: s.readonly}
	} else if s.readonly && !opts.ReadOnly {
		return nil, errors.ErrReadOnly
	}

	// Pick the pool from the kind of transaction being opened. go-sqlite3 discards
	// sql.TxOptions entirely (SQLiteConn.BeginTx ignores its argument and issues the
	// connection's own txlock statement), so the DSN of the connection is the only place
	// the choice between BEGIN and BEGIN IMMEDIATE can be made.
	conn := s.read
	if !opts.ReadOnly {
		conn = s.writer()
	}

	var tx *sql.Tx
	if tx, err = conn.BeginTx(ctx, opts); err != nil {
		return nil, err
	}

	// Get the actor's ID and type. If unset, use an "unknown" actor so that
	// database transactions do not fail at the audit log creation step.
	var (
		actorID   []byte
		actorType enum.Actor
		ok        bool
	)
	if actorID, ok = audit.ActorID(ctx); !ok {
		actorID = []byte("unknown")
	}
	if actorType, ok = audit.ActorType(ctx); !ok {
		actorType = enum.ActorUnknown
	}

	return &Tx{
		tx:        tx,
		opts:      opts,
		mkta:      s.mkta,
		actorID:   actorID,
		actorType: actorType,
	}, nil
}

func (s *Store) UseTravelAddressFactory(f models.TravelAddressFactory) {
	s.mkta = f
}

// Stats reports the two pools merged into a single sql.DBStats: every field is the sum
// of the read pool's and the write pool's, so OpenConnections is the total the store
// holds against the database file and MaxOpenConnections is the combined ceiling (0
// while neither pool is capped, which is the default). The operator-facing DBInfo
// endpoint wants the store's total footprint, not one pool's, and a reader saturating
// its pool is as interesting as a writer saturating its own.
func (s *Store) Stats() sql.DBStats {
	stats := s.read.Stats()
	if s.write == nil {
		return stats
	}

	write := s.write.Stats()
	stats.MaxOpenConnections += write.MaxOpenConnections
	stats.OpenConnections += write.OpenConnections
	stats.InUse += write.InUse
	stats.Idle += write.Idle
	stats.WaitCount += write.WaitCount
	stats.WaitDuration += write.WaitDuration
	stats.MaxIdleClosed += write.MaxIdleClosed
	stats.MaxIdleTimeClosed += write.MaxIdleTimeClosed
	stats.MaxLifetimeClosed += write.MaxLifetimeClosed

	return stats
}

//===========================================================================
// Tx methods
//===========================================================================

func (t *Tx) Commit() error {
	return t.tx.Commit()
}

func (t *Tx) Rollback() error {
	return t.tx.Rollback()
}

// Sets the actor metadata to be returned by [Tx.GetActor].
func (t *Tx) SetActor(actorID []byte, actorType enum.Actor) {
	t.actorID = actorID
	t.actorType = actorType
}

// Returns the actor metadata set by [Tx.SetActor], or from the context in
// [Store.BeginTx].
func (t *Tx) GetActor() ([]byte, enum.Actor) {
	return t.actorID, t.actorType
}

func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.tx.Query(query, args...)
}

func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.tx.QueryRow(query, args...)
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}
