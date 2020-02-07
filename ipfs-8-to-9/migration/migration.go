// package mg8 contains the code to perform 8-9 repository migration in
// go-ipfs. This performs a switch to raw multihashes for all keys in the
// go-ipfs datastore (https://github.com/ipfs/go-ipfs/issues/6815).
package mg8

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"

	migrate "github.com/ipfs/fs-repo-migrations/go-migrate"
	lock "github.com/ipfs/fs-repo-migrations/ipfs-1-to-2/repolock"
	"github.com/ipfs/fs-repo-migrations/mfsr"
	"github.com/ipfs/go-filestore"
	dshelp "github.com/ipfs/go-ipfs-ds-help"

	log "github.com/ipfs/fs-repo-migrations/stump"
	ds "github.com/ipfs/go-datastore"
	"github.com/ipfs/go-ipfs/plugin/loader"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"
)

const backupFile = "8-to-9-cids.txt"

var migrationPrefixes = []ds.Key{
	ds.NewKey("blocks"),
	filestore.FilestorePrefix,
}

// Migration implements the migration described above.
type Migration struct{}

// Versions returns the current version string for this migration.
func (m Migration) Versions() string {
	return "8-to-9"
}

// Reversible returns true.
func (m Migration) Reversible() bool {
	return true
}

// lock the repo
func (m Migration) lock(opts migrate.Options) (io.Closer, error) {
	log.VLog("locking repo at %q", opts.Path)
	return lock.Lock2(opts.Path)
}

// open the repo
func (m Migration) open(opts migrate.Options) (ds.Batching, error) {
	log.VLog("  - loading repo configurations")
	plugins, err := loader.NewPluginLoader(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("error loading plugins: %s", err)
	}

	if err := plugins.Initialize(); err != nil {
		return nil, fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return nil, fmt.Errorf("error injecting plugins: %s", err)
	}

	cfg, err := fsrepo.ConfigAt(opts.Path)
	if err != nil {
		return nil, err
	}

	dsc, err := fsrepo.AnyDatastoreConfig(cfg.Datastore.Spec)
	if err != nil {
		return nil, err
	}

	return dsc.Create(opts.Path)
}

// Apply runs the migration and writes a log file that can be used by Revert.
func (m Migration) Apply(opts migrate.Options) error {
	log.Verbose = opts.Verbose
	log.Log("applying %s repo migration", m.Versions())

	lk, err := m.lock(opts)
	if err != nil {
		return err
	}
	defer lk.Close()

	repo := mfsr.RepoPath(opts.Path)

	log.VLog("  - verifying version is '8'")
	if err := repo.CheckVersion("8"); err != nil {
		return err
	}

	dstore, err := m.open(opts)
	if err != nil {
		return err
	}
	defer dstore.Close()

	log.VLog("  - starting CIDv1 to raw multihash block migration")

	// Prepare backing up of CIDs
	backupPath := filepath.Join(opts.Path, backupFile)
	log.VLog("  - backup file will be written to %s", backupPath)
	_, err = os.Stat(backupPath)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Error(err)
			return err
		}
	} else { // backup file exists
		log.Log("WARN: backup file %s already exists. CIDs-Multihash pairs will be appended", backupPath)
	}

	// If it exists, append to it.
	f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0644)
	if err != nil {
		log.Error(err)
		return err
	}
	defer f.Close()
	buf := bufio.NewWriter(f)

	swapCh := make(chan Swap, 1000)

	writingDone := make(chan struct{})
	go func() {
		for sw := range swapCh {
			// Only write the Old string (a CID). We can derive
			// the multihash from it.
			fmt.Fprint(buf, sw.Old.String(), "\n")
		}
		close(writingDone)
	}()

	// Add all the keys to migrate to the backup file
	for _, prefix := range migrationPrefixes {
		log.VLog("  - Adding keys in prefix %s to backup file", prefix)
		cidSwapper := CidSwapper{Prefix: prefix, Store: dstore, SwapCh: swapCh}
		total, err := cidSwapper.Run(true) // DRY RUN
		if err != nil {
			close(swapCh)
			log.Error(err)
			return err
		}
		log.Log("%d CIDv1 keys added to backup file for %s", total, prefix)
	}
	close(swapCh)
	// Wait for our writing to finish before doing the flushing.
	<-writingDone
	buf.Flush()

	// The backup file is ready. Run the migration.
	for _, prefix := range migrationPrefixes {
		log.VLog("  - Migrating keys in prefix %s", prefix)
		cidSwapper := CidSwapper{Prefix: prefix, Store: dstore}
		total, err := cidSwapper.Run(false) // NOT a Dry Run
		if err != nil {
			log.Error(err)
			return err
		}
		log.Log("%d CIDv1 keys in %s have been migrated", total, prefix)
	}

	if err := repo.WriteVersion("9"); err != nil {
		log.Error("failed to write version file")
		return err
	}
	log.Log("updated version file")

	return nil
}

// Revert attempts to undo the migration using the log file written by Apply.
func (m Migration) Revert(opts migrate.Options) error {
	log.Verbose = opts.Verbose
	log.Log("reverting %s repo migration", m.Versions())

	lk, err := m.lock(opts)
	if err != nil {
		return err
	}
	defer lk.Close()

	repo := mfsr.RepoPath(opts.Path)

	log.VLog("  - verifying version is '9'")
	if err := repo.CheckVersion("9"); err != nil {
		return err
	}

	log.VLog("  - starting raw multihash to CIDv1 block migration")
	dstore, err := m.open(opts)
	if err != nil {
		return err
	}
	defer dstore.Close()

	// Open revert path for reading
	backupPath := filepath.Join(opts.Path, backupFile)
	log.VLog("  - backup file will be read from %s", backupPath)
	f, err := os.Open(backupPath)
	if err != nil {
		log.Error(err)
		return err
	}

	unswapCh := make(chan Swap, 1000)
	scanner := bufio.NewScanner(f)
	var scannerErr error

	go func() {
		defer close(unswapCh)

		for scanner.Scan() {
			cidPath := ds.NewKey(scanner.Text())
			cidKey := ds.NewKey(cidPath.BaseNamespace())
			prefix := cidPath.Parent()
			cid, err := dsKeyToCid(cidKey)
			if err != nil {
				log.Error("could not parse cid from backup file: %s", err)
				scannerErr = err
				break
			}
			mhashPath := prefix.Child(dshelp.MultihashToDsKey(cid.Hash()))
			// This is the original swap object which is what we
			// wanted to rebuild. Old is the old path and new is
			// the new path and the unswapper will revert this.
			sw := Swap{Old: cidPath, New: mhashPath}
			unswapCh <- sw
		}
		if err := scanner.Err(); err != nil {
			log.Error(err)
			return
		}

	}()

	// The backup file contains prefixed keys, so we do not need to set
	// them.
	cidSwapper := CidSwapper{Store: dstore}
	total, err := cidSwapper.Revert(unswapCh)
	if err != nil {
		log.Error(err)
		return err
	}
	// Revert will only return after unswapCh is closed, so we know
	// scannerErr is safe to read at this point.
	if scannerErr != nil {
		return err
	}

	log.Log("%d multihashes reverted to CidV1s", total)
	if err := repo.WriteVersion("8"); err != nil {
		log.Error("failed to write version file")
		return err
	}

	log.Log("reverted version file to version 8")
	err = f.Close()
	if err != nil {
		log.Error("could not close backup file")
		return err
	}
	err = os.Rename(backupPath, backupPath+".reverted")
	if err != nil {
		log.Error("could not rename the backup file, but migration worked: %s", err)
		return err
	}
	return nil
}
