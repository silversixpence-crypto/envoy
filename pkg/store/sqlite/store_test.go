package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trisacrypto/envoy/pkg/audit"
	"github.com/trisacrypto/envoy/pkg/enum"
	"github.com/trisacrypto/envoy/pkg/store/dsn"
	dberr "github.com/trisacrypto/envoy/pkg/store/errors"
	"github.com/trisacrypto/envoy/pkg/store/models"
	db "github.com/trisacrypto/envoy/pkg/store/sqlite"
	"github.com/trisacrypto/envoy/pkg/trisa/keychain"
	"go.rtnl.ai/ulid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/trisacrypto/trisa/pkg/ivms101"
	trisa "github.com/trisacrypto/trisa/pkg/trisa/api/v1beta1"
	generic "github.com/trisacrypto/trisa/pkg/trisa/data/generic/v1beta1"
	"github.com/trisacrypto/trisa/pkg/trisa/keys"
	"github.com/trisacrypto/trisa/pkg/trust"

	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

func TestConnectClose(t *testing.T) {
	t.Run("ReadWrite", func(t *testing.T) {
		uri, _ := dsn.Parse("sqlite3:///" + filepath.Join(t.TempDir(), "test.db"))

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")

		tx, err := store.BeginTx(context.Background(), nil)
		require.NoError(t, err, "could not create write transaction")
		tx.Rollback()

		tx, err = store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err, "could not create readonly transaction")
		tx.Rollback()

		err = store.Close()
		require.NoError(t, err, "should be able to close the db without error when not connected")
	})

	t.Run("ReadOnly", func(t *testing.T) {
		// NOTE: a readonly DSN opens sqlite with mode=ro, which cannot create the
		// database file, so the database has to be created before it can be reopened.
		path := filepath.Join(t.TempDir(), "test.db")
		createURI, _ := dsn.Parse("sqlite3:///" + path)

		created, err := db.Open(createURI)
		require.NoError(t, err, "could not create temporary sqlite database")
		require.NoError(t, created.Close(), "could not close temporary sqlite database")

		uri, _ := dsn.Parse("sqlite3:///" + path + "?readonly=true")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")

		tx, err := store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: false})
		require.ErrorIs(t, err, dberr.ErrReadOnly, "created write transaction in readonly mode")
		require.Nil(t, tx, "expected no transaction to be returned")

		tx, err = store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err, "could not create readonly transaction")
		tx.Rollback()

		err = store.Close()
		require.NoError(t, err, "should be able to close the db without error when not connected")
	})

	t.Run("Failures", func(t *testing.T) {
		tests := []struct {
			uri *dsn.DSN
			err error
		}{
			{
				&dsn.DSN{Scheme: "leveldb"},
				dberr.ErrUnknownScheme,
			},
			{
				&dsn.DSN{Scheme: "sqlite3"},
				dberr.ErrPathRequired,
			},
		}

		for i, tc := range tests {
			_, err := db.Open(tc.uri)
			require.ErrorIs(t, err, tc.err, "test case %d failed", i)
		}
	})
}

// The hosted image replicates the database with Litestream, which requires WAL mode and
// a busy timeout, so the connection parameters have to reach the driver and the
// operator has to be able to override them from the DSN.
func TestConnectionParams(t *testing.T) {
	t.Run("Defaults", func(t *testing.T) {
		uri, _ := dsn.Parse("sqlite3:///" + filepath.Join(t.TempDir(), "test.db"))

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")
		defer store.Close()

		require.Equal(t, "wal", pragma(t, store, "PRAGMA journal_mode"), "expected WAL journal mode for litestream replication")
		require.Equal(t, "5000", pragma(t, store, "PRAGMA busy_timeout"), "expected a busy timeout so checkpoint locks wait instead of erroring")
		require.Equal(t, "1", pragma(t, store, "PRAGMA synchronous"), "expected synchronous NORMAL (1) to pair with WAL")
		require.Equal(t, "1", pragma(t, store, "PRAGMA foreign_keys"), "expected foreign key enforcement")
	})

	t.Run("EveryPooledConnection", func(t *testing.T) {
		uri, _ := dsn.Parse("sqlite3:///" + filepath.Join(t.TempDir(), "test.db"))

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")
		defer store.Close()

		// Hold a transaction open so that the pool has to hand out a second, fresh
		// connection: the one-off PRAGMA in Open never reached that connection.
		held, err := store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err, "could not open the first transaction")
		defer held.Rollback()

		tx, err := store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err, "could not open a second transaction on a fresh connection")
		defer tx.Rollback()

		var foreignKeys string
		require.NoError(t, tx.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys), "could not query pragma")
		require.Equal(t, "1", foreignKeys, "expected foreign keys on every pooled connection")
	})

	t.Run("Overrides", func(t *testing.T) {
		uri, _ := dsn.Parse("sqlite3:///" + filepath.Join(t.TempDir(), "test.db") + "?_journal_mode=DELETE&_busy_timeout=250")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")
		defer store.Close()

		require.Equal(t, "delete", pragma(t, store, "PRAGMA journal_mode"), "expected the dsn journal mode to win over the default")
		require.Equal(t, "250", pragma(t, store, "PRAGMA busy_timeout"), "expected the dsn busy timeout to win over the default")
		require.Equal(t, "1", pragma(t, store, "PRAGMA foreign_keys"), "expected untouched defaults to still apply")
	})

	t.Run("ReadOnly", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		createURI, _ := dsn.Parse("sqlite3:///" + path)

		created, err := db.Open(createURI)
		require.NoError(t, err, "could not create temporary sqlite database")
		require.NoError(t, created.Close(), "could not close temporary sqlite database")

		uri, _ := dsn.Parse("sqlite3:///" + path + "?readonly=true")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")
		defer store.Close()

		tx, err := store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
		require.NoError(t, err, "could not create readonly transaction")
		defer tx.Rollback()

		_, err = tx.Exec("CREATE TABLE readonly_check (id INTEGER PRIMARY KEY)")
		require.ErrorContains(t, err, "readonly database", "expected sqlite to refuse the write, not just the store")
	})

	t.Run("ReadOnlyKeepsJournalMode", func(t *testing.T) {
		// A database created before the WAL default uses a rollback journal; opening
		// it read only must not try to switch it to WAL, which is a write.
		path := filepath.Join(t.TempDir(), "test.db")
		createURI, _ := dsn.Parse("sqlite3:///" + path + "?_journal_mode=DELETE")

		created, err := db.Open(createURI)
		require.NoError(t, err, "could not create rollback-journal sqlite database")
		require.Equal(t, "delete", pragma(t, created, "PRAGMA journal_mode"), "expected a rollback-journal database")
		require.NoError(t, created.Close(), "could not close temporary sqlite database")

		uri, _ := dsn.Parse("sqlite3:///" + path + "?readonly=true")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not reopen a rollback-journal database read only")
		defer store.Close()

		require.Equal(t, "delete", pragma(t, store, "PRAGMA journal_mode"), "read only open must keep the existing journal mode")
	})

	t.Run("EscapesPath", func(t *testing.T) {
		// dsn.Parse decodes the path, so a filename with URI metacharacters has to be
		// re-escaped when the file: URI is built or the driver opens a different file.
		path := filepath.Join(t.TempDir(), "odd#name?.db")
		uri, _ := dsn.Parse("sqlite3:///" + strings.NewReplacer("#", "%23", "?", "%3F").Replace(path))
		require.Equal(t, path, uri.Path, "expected the dsn to decode the path")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open a database whose name has uri metacharacters")
		require.NoError(t, store.Close(), "could not close the database")

		_, err = os.Stat(path)
		require.NoError(t, err, "expected the database to be created at the decoded path")
	})

	t.Run("Checkpoint", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "test.db")
		uri, _ := dsn.Parse("sqlite3:///" + path)

		store, err := db.Open(uri)
		require.NoError(t, err, "could not create temporary sqlite database")
		require.Equal(t, "wal", pragma(t, store, "PRAGMA journal_mode"))
		require.NoError(t, store.Close(), "could not close temporary sqlite database")

		require.NoError(t, db.Checkpoint(path), "could not checkpoint the database")

		info, err := os.Stat(path + "-wal")
		require.True(t, err != nil || info.Size() == 0, "expected the write-ahead log to be empty or gone after a checkpoint")
	})
}

// Most write transactions in this package read before they write. A deferred BEGIN takes
// only a read lock at the first SELECT, so the first write has to upgrade it, and in WAL
// mode SQLite refuses that upgrade with SQLITE_BUSY_SNAPSHOT as soon as another
// connection has committed since the snapshot was taken. It is not a wait that _busy_-
// timeout can absorb: the snapshot is already stale, so the busy handler is never
// consulted and the caller sees "database is locked" immediately. The store opens write
// transactions on a pool that begins them with BEGIN IMMEDIATE instead, which turns the
// failure into ordinary lock contention that the busy handler waits out.
func TestConcurrentReadThenWrite(t *testing.T) {
	const (
		workers = 8
		txns    = 40
	)

	// Runs workers*txns read-then-write transactions against a fresh database opened
	// with the given DSN query string and returns every error they produced.
	probe := func(t *testing.T, options string) (errs []error) {
		t.Helper()

		uri, err := dsn.Parse("sqlite3:///" + filepath.Join(t.TempDir(), "test.db") + options)
		require.NoError(t, err, "could not parse the dsn")

		store, err := db.Open(uri)
		require.NoError(t, err, "could not open connection to temporary sqlite database")
		defer store.Close()

		setup, err := store.BeginTx(context.Background(), nil)
		require.NoError(t, err, "could not open the setup transaction")
		defer setup.Rollback()

		_, err = setup.Exec("CREATE TABLE lock_probe (id INTEGER PRIMARY KEY, val INTEGER NOT NULL)")
		require.NoError(t, err, "could not create the scratch table")

		_, err = setup.Exec("INSERT INTO lock_probe (val) VALUES (0)")
		require.NoError(t, err, "could not seed the scratch table")
		require.NoError(t, setup.Commit(), "could not commit the setup transaction")

		var (
			mu    sync.Mutex
			wg    sync.WaitGroup
			start = make(chan struct{})
		)

		for i := 0; i < workers; i++ {
			wg.Add(1)

			go func() {
				defer wg.Done()
				<-start

				for j := 0; j < txns; j++ {
					if err := readThenWrite(store); err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
			}()
		}

		// Release the workers together so that the transactions actually overlap.
		close(start)
		wg.Wait()

		return errs
	}

	t.Run("Immediate", func(t *testing.T) {
		errs := probe(t, "")
		require.Empty(t, errs, "expected every write transaction to commit, got %d failures (first: %v)", len(errs), firstErr(errs))
	})

	t.Run("Deferred", func(t *testing.T) {
		// The same workload with the driver's default deferred BEGIN, which is what the
		// store did before the pools were split. This documents the mechanism: the
		// failures here are exactly the ones the immediate write pool removes.
		errs := probe(t, "?_txlock=deferred")
		t.Logf("%d of %d deferred transactions failed the lock upgrade", len(errs), workers*txns)
		require.NotEmpty(t, errs, "expected deferred transactions to fail the lock upgrade")
		require.ErrorContains(t, firstErr(errs), "database is locked", "expected the failures to be lock upgrade failures")
	})
}

// One write transaction that reads before it writes, which is the shape every
// read-modify-write store method in this package has.
func readThenWrite(store *db.Store) (err error) {
	var tx *db.Tx
	if tx, err = store.BeginTx(context.Background(), nil); err != nil {
		return err
	}

	defer tx.Rollback()

	var val int
	if err = tx.QueryRow("SELECT val FROM lock_probe ORDER BY id DESC LIMIT 1").Scan(&val); err != nil {
		return err
	}

	if _, err = tx.Exec("INSERT INTO lock_probe (val) VALUES (?)", val+1); err != nil {
		return err
	}

	return tx.Commit()
}

func firstErr(errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	return errs[0]
}

// Returns the first column of the given pragma query as a string.
func pragma(t *testing.T, store *db.Store, query string) string {
	t.Helper()

	tx, err := store.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err, "could not create readonly transaction")
	defer tx.Rollback()

	var value string
	require.NoError(t, tx.QueryRow(query).Scan(&value), "could not query %s", query)
	return value
}

//===========================================================================
// Store Test Suite
//===========================================================================

type storeTestSuite struct {
	suite.Suite
	dbpath string
	store  *db.Store
}

func (s *storeTestSuite) SetupSuite() {
	s.CreateDB()
	loadAuditKeyChainFixture(s.T())
}

// Reset the DB before every test.
func (s *storeTestSuite) SetupTest() {
	s.ResetDB()
}

// Reset the DB before every subtest.
func (s *storeTestSuite) SetupSubtest() {
	s.ResetDB()
}

func (s *storeTestSuite) CreateDB() {
	var err error
	require := s.Require()

	// Only create the database path on the first call to CreateDB. Otherwise the call
	// to TempDir() will be prefixed with the name of the subtest, which will cause an
	// "attempt to write a read-only database" for subsequent tests because the directory
	// will be deleted when the subtest is complete.
	if s.dbpath == "" {
		s.dbpath = filepath.Join(s.T().TempDir(), "envoytests.db")
	}

	uri, _ := dsn.Parse("sqlite3:///" + s.dbpath)
	s.store, err = db.Open(uri)
	require.NoError(err, "could not open store in temporary location")

	// Execute any SQL files in the testdata directory
	paths, err := filepath.Glob("testdata/*.sql")
	require.NoError(err, "could not list testdata directory")

	tx, err := s.store.BeginTx(context.Background(), nil)
	require.NoError(err, "could not open transaction")
	defer tx.Rollback()

	for _, path := range paths {
		stmt, err := os.ReadFile(path)
		require.NoError(err, "could not read query from file")

		_, err = tx.Exec(string(stmt))
		require.NoError(err, "could not execute sql query from fixture %s", path)
	}

	require.NoError(tx.Commit(), "could not commit transaction")
}

func (s *storeTestSuite) ResetDB() {
	require := s.Require()
	require.NoError(s.store.Close(), "could not close connection to db")
	require.NoError(os.Remove(s.dbpath), "could not delete old database")
	s.CreateDB()
}

func TestStore(t *testing.T) {
	suite.Run(t, new(storeTestSuite))
}

//===========================================================================
// Helper Functions
//===========================================================================

func loadPayload(identityPath, transactionPath string) (payload *trisa.Payload, err error) {
	payload = &trisa.Payload{}

	payload.Identity, _ = anypb.New(&ivms101.IdentityPayload{})
	if err = loadFixture(identityPath, payload.Identity); err != nil {
		return nil, err
	}

	payload.Transaction, _ = anypb.New(&generic.Transaction{})
	if err = loadFixture(transactionPath, payload.Transaction); err != nil {
		return nil, err
	}

	payload.SentAt = time.Now().UTC().Format(time.RFC3339)
	return payload, nil
}

func loadFixture(path string, obj proto.Message) (err error) {
	var data []byte
	if data, err = os.ReadFile(path); err != nil {
		return err
	}

	json := protojson.UnmarshalOptions{
		AllowPartial:   true,
		DiscardUnknown: true,
	}

	return json.Unmarshal(data, obj)
}

func loadAuditKeyChainFixture(t *testing.T) {
	// Load Certificate fixture with private keys
	sz, err := trust.NewSerializer(false)
	require.NoError(t, err, "could not create serializer to load fixture")

	provider, err := sz.ReadFile("testdata/certs.pem")
	require.NoError(t, err, "could not read test fixture")

	certs, err := keys.FromProvider(provider)
	require.NoError(t, err, "could not create Key from provider")
	require.True(t, certs.IsPrivate(), "expected test certs fixture to be private")

	// Setup a mock KeyChain
	kc, err := keychain.New(keychain.WithCacheDuration(1*time.Hour), keychain.WithDefaultKey(certs))
	require.NoError(t, err, "could not create a KeyChain")
	audit.UseKeyChain(kc)
}

// Returns a context.Background() with ActorID and ActorType context values for
// audit log testing. The ActorID is a fresh, random ULID and the type is
// an enum.ActorAPIKey.
func (s *storeTestSuite) ActorContext() context.Context {
	return audit.WithActor(context.Background(), ulid.MakeSecure().Bytes(), enum.ActorAPIKey)
}

// Counts the audit logs (by action and resource type) created recently (1
// hour) and compares those counts to the expected counts. The expected map
// takes a string from the function ActionResourceKey() for indexing.
func (s *storeTestSuite) AssertAuditLogCount(expected map[string]int) bool {
	// setup
	require := s.Require()
	ctx := s.ActorContext()
	pageInfo := &models.ComplianceAuditLogPageInfo{
		After:  time.Now().Add(-1 * time.Hour),
		Before: time.Now(),
	}

	// get logs
	logs, err := s.store.ListComplianceAuditLogs(ctx, pageInfo)
	require.NoError(err, "error getting logs")
	require.NotNil(logs, "logs was nil")

	// count logs
	actual := make(map[string]int, len(expected))
	for _, log := range logs.Logs {
		actual[ActionResourceKey(log.Action, log.ResourceType)]++
	}

	// compare
	require.Equal(expected, actual, "audit log count is off")
	return reflect.DeepEqual(expected, actual)
}

// Returns a string that can be used in the expected map for ExpectedAuditLogs().
func ActionResourceKey(a enum.Action, r enum.Resource) string {
	return a.String() + r.String()
}
