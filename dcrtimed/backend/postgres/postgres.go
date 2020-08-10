// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package postgres

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/decred/dcrtime/dcrtimed/backend"
	"github.com/decred/dcrtime/dcrtimed/dcrtimewallet"
	_ "github.com/lib/pq"
	"github.com/robfig/cron"
)

const (
	tableRecords = "records"
	tableAnchors = "anchors"
)

var (
	_ backend.Backend = (*Postgres)(nil)

	// duration and flushSchedule must match or bad things will happen. By
	// matching we mean both are hourly or every so many minutes.
	//
	// Seconds Minutes Hours Days Months DayOfWeek
	flushSchedule = "10 0 * * * *" // On the hour + 10 seconds
	duration      = time.Hour      // Default how often we combine digests
)

// Postgres is a postgreSQL implementation of a backend, it stores all uploaded
// digests in records table, on flush it stores all anchor info as well and
// link all anchored records with the corresponding anchor.
type Postgres struct {
	sync.RWMutex

	cron     *cron.Cron    // Scheduler for periodic tasks
	db       *sql.DB       // Postgres database
	duration time.Duration // How often we combine digests
	commit   uint          // Current version, incremented during flush

	enableCollections bool // Set to true to enable collection query

	wallet *dcrtimewallet.DcrtimeWallet // Wallet context.

	// testing only entries
	myNow   func() time.Time // Override time.Now()
	testing bool             // Enabled during test
}

// Return timestamp information for given digests.
func (pg *Postgres) Get([][sha256.Size]byte) ([]backend.GetResult, error) {
	return nil, nil
}

// Return all hashes for given timestamps.
func (pg *Postgres) GetTimestamps([]int64) ([]backend.TimestampResult, error) {
	return nil, nil
}

// Store hashes and return timestamp and associated errors.  Put is
// allowed to return transient errors.
func (pg *Postgres) Put([][sha256.Size]byte) (int64, []backend.PutResult, error) {
	return 0, nil, nil
}

// Close performs cleanup of the backend.
func (pg *Postgres) Close() {
}

// Dump dumps database to the provided file descriptor. If the
// human flag is set to true it pretty prints the database content
// otherwise it dumps a JSON stream.
func (pg *Postgres) Dump(*os.File, bool) error {
	return nil
}

// Restore recreates the the database from the provided file
// descriptor. The verbose flag is set to true to indicate that this
// call may parint to stdout. The provided string describes the target
// location and is implementation specific.
func (pg *Postgres) Restore(*os.File, bool, string) error {
	return nil
}

// Fsck walks all data and verifies its integrity. In addition it
// verifies anchored timestamps' existence on the blockchain.
func (pg *Postgres) Fsck(*backend.FsckOptions) error {
	return nil
}

// GetBalance retrieves balance information for the wallet
// backing this instance
func (pg *Postgres) GetBalance() (*backend.GetBalanceResult, error) {
	return nil, nil
}

// LastAnchor retrieves last successful anchor details
func (pg *Postgres) LastAnchor() (*backend.LastAnchorResult, error) {
	return nil, nil
}

func buildQueryString(rootCert, cert, key string) string {
	v := url.Values{}
	v.Set("sslmode", "require")
	v.Set("sslrootcert", filepath.Clean(rootCert))
	v.Set("sslcert", filepath.Join(cert))
	v.Set("sslkey", filepath.Join(key))
	return v.Encode()
}

func hasTable(db *sql.DB, name string) (bool, error) {
	rows, err := db.Query(`SELECT EXISTS (SELECT FROM information_schema.tables 
		WHERE table_schema = 'public' AND table_name  = $1)`, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var exists bool
	for rows.Next() {
		err = rows.Scan(&exists)
		if err != nil {
			return false, err
		}
	}
	return exists, nil
}

func createAnchorsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE public.anchors
(
    merkle character varying(64) COLLATE pg_catalog."default" NOT NULL,
    hashes text[] COLLATE pg_catalog."default" NOT NULL,
    tx_hash text COLLATE pg_catalog."default",
    chain_timestamp bigint,
    flush_timestamp bigint,
    CONSTRAINT anchors_pkey PRIMARY KEY (merkle)
);
-- Index: idx_chain_timestamp
CREATE INDEX idx_chain_timestamp
    ON public.anchors USING btree
    (chain_timestamp ASC NULLS LAST)
    TABLESPACE pg_default;
-- Index: idx_flush_timestamp
CREATE INDEX idx_flush_timestamp
    ON public.anchors USING btree
    (flush_timestamp ASC NULLS LAST)
    TABLESPACE pg_default;
`)
	if err != nil {
		return err
	}
	log.Infof("Anchors table created")
	return nil
}

func createRecordsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE public.records
(
    digest bytea NOT NULL,
    anchor_merkle character varying(64) COLLATE pg_catalog."default",
    key serial NOT NULL,
    collection_timestamp text COLLATE pg_catalog."default" NOT NULL,
    CONSTRAINT records_pkey PRIMARY KEY (key),
    CONSTRAINT records_anchors_fkey FOREIGN KEY (anchor_merkle)
        REFERENCES public.anchors (merkle) MATCH SIMPLE
        ON UPDATE NO ACTION
        ON DELETE NO ACTION
        NOT VALID
);

-- Index: fki_records_anchors_fkey
CREATE INDEX fki_records_anchors_fkey
    ON public.records USING btree
    (anchor_merkle COLLATE pg_catalog."default" ASC NULLS LAST)
    TABLESPACE pg_default;

-- Index: idx_collection_timestamp
CREATE INDEX idx_collection_timestamp
    ON public.records USING btree
    (collection_timestamp COLLATE pg_catalog."default" ASC NULLS LAST)
    TABLESPACE pg_default;
`)
	if err != nil {
		return err
	}
	log.Infof("Records table created")
	return nil
}

func createTables(db *sql.DB) error {
	exists, err := hasTable(db, tableAnchors)
	if err != nil {
		return err
	}
	if !exists {
		err = createAnchorsTable(db)
		if err != nil {
			return err
		}
	}
	exists, err = hasTable(db, tableRecords)
	if err != nil {
		return err
	}
	if !exists {
		err = createRecordsTable(db)
		if err != nil {
			return err
		}
	}
	return nil
}

// internalNew creates the Pstgres context but does not launch background
// bits.  This is used by the test packages.
func internalNew(user, host, net, rootCert, cert, key string) (*Postgres, error) {
	// Connect to database
	dbName := net + "_dcrtime"
	h := "postgresql://" + user + "@" + host + "/" + dbName
	u, err := url.Parse(h)
	if err != nil {
		return nil, fmt.Errorf("parse url '%v': %v", h, err)
	}

	qs := buildQueryString(rootCert, cert, key)
	addr := u.String() + "?" + qs

	db, err := sql.Open("postgres", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to database '%v': %v", addr, err)
	}

	// Create tables
	err = createTables(db)
	if err != nil {
		return nil, err
	}

	pg := &Postgres{
		cron:     cron.New(),
		db:       db,
		duration: duration,
		myNow:    time.Now,
	}

	return pg, nil
}

// New creates a new backend instance.  The caller should issue a Close once
// the Postgres backend is no longer needed.
func New(user, host, net, rootCert, cert, key, walletCert, walletHost string, enableCollections bool, walletPassphrase []byte) (*Postgres, error) {
	// XXX log more stuff
	log.Tracef("New: %v %v %v %v %v %v", user, host, net, rootCert, cert, key)

	pg, err := internalNew(user, host, net, rootCert, cert, key)
	if err != nil {
		return nil, err
	}
	pg.enableCollections = enableCollections

	// Runtime bits
	dcrtimewallet.UseLogger(log)
	pg.wallet, err = dcrtimewallet.New(walletCert, walletHost, walletPassphrase)
	if err != nil {
		return nil, err
	}

	// Flushing backend reconciles uncommitted work to the global database.
	//start := time.Now()
	//flushed, err := pg.doFlush()
	//end := time.Since(start)
	//if err != nil {
	//return nil, err
	//}

	//if flushed != 0 {
	//log.Infof("Startup flusher: directories %v in %v", flushed, end)
	//}

	// Launch cron.
	err = pg.cron.AddFunc(flushSchedule, func() {
	})
	if err != nil {
		return nil, err
	}

	pg.cron.Start()

	return pg, nil
}
