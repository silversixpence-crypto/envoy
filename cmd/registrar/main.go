// Command registrar writes a single VASP record into a TRISA directory
// (GDS) LevelDB store. It is run as a one-shot container by the TR DataDrop
// platform api to register each newly provisioned Envoy node in our own
// directory.
//
// It mirrors the store-opening pattern from cmd/fsi/main.go (connectDB) and
// the protojson-unmarshal pattern from cmd/fsi/localhost.go.
//
//	registrar -db leveldb:////data/db -in /in/vasp.json
//
// Note the four slashes: the directory package's ParseDSN trims one leading
// slash from the path, so leveldb:///data/db would open data/db relative to the
// working directory rather than the /data volume.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/trisacrypto/directory/pkg/gds/config"
	"github.com/trisacrypto/directory/pkg/store"
	dbconf "github.com/trisacrypto/directory/pkg/store/config"
	"github.com/trisacrypto/directory/pkg/utils/logger"
	pb "github.com/trisacrypto/trisa/pkg/trisa/gds/models/v1beta1"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() {
	dbURL := flag.String("db", "leveldb:////data/db", "directory store DSN (absolute paths need four slashes)")
	inPath := flag.String("in", "/in/vasp.json", "path to the VASP protojson file to load")
	flag.Parse()

	// Silence the directory package's global logger.
	logger.Discard()

	// Configure and open the directory store exactly like cmd/fsi connectDB.
	conf := config.Config{
		DirectoryID: "trisatest.dev",
		Maintenance: true,
		Database: dbconf.StoreConfig{
			URL:           *dbURL,
			ReindexOnBoot: false,
			Insecure:      true,
		},
	}

	db, err := store.Open(conf.Database)
	if err != nil {
		fatal("could not open store: %v", err)
	}
	defer db.Close()

	// Read the VASP protojson fixture from disk.
	data, err := os.ReadFile(*inPath)
	if err != nil {
		fatal("could not read input %s: %v", *inPath, err)
	}

	// Unmarshal protojson into a pb.VASP exactly like cmd/fsi unmarshalPBFixture.
	vasp := new(pb.VASP)
	opts := protojson.UnmarshalOptions{
		AllowPartial:   true,
		DiscardUnknown: false,
	}

	if err = opts.Unmarshal(data, vasp); err != nil {
		fatal("could not unmarshal vasp protojson: %v", err)
	}

	// Create the VASP record and print its id (stdout, parseable by the caller).
	id, err := db.CreateVASP(context.Background(), vasp)
	if err != nil {
		fatal("could not create vasp record: %v", err)
	}

	fmt.Printf("created vasp record with id: %s\n", id)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
