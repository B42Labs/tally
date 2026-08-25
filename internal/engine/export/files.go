package export

import (
	"fmt"
	"os"
	"path/filepath"
)

// dirMode is what an output directory is created with. Billing artifacts name
// every project of an installation and what it was invoiced, so nothing here is
// readable by anyone but the user the export ran as. The files keep the 0o600
// os.CreateTemp gives them, which says the same.
const dirMode = 0o700

// prepareDir creates the directory a writer writes into, with every parent it
// needs. A --out that names an existing file surfaces here, as the ENOTDIR the
// wrapped *fs.PathError carries.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating the output directory %s: %w", dir, err)
	}
	return nil
}

// writeFile writes one artifact into dir under name. The bytes go to a
// temporary file in the same directory and are renamed onto the final name, so
// a reader picking files up out of the directory never sees a half-written one,
// and an earlier export's file is replaced in the one rename rather than
// truncated and filled in. A failure at any step takes the temporary file with
// it and names the final path, which is the one an operator asked for.
//
// The name is joined rather than sanitized: DocumentFileName escapes every
// slash out of a statement key, so no name a key produces holds a path
// separator, and the other names are constants.
func writeFile(dir, name string, body []byte) error {
	path := filepath.Join(dir, name)
	// The pattern keeps the artifact's name in front, so a temporary file a
	// killed process leaves behind says which export it belonged to.
	tmp, err := os.CreateTemp(dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// The bytes are put on stable storage before the name exists. Close alone
	// flushes them to the page cache, so a node that loses power after the
	// rename would come back holding a file under its final name with nothing,
	// or half of a document, in it: a reader that takes the name for a complete
	// file would ingest an empty invoice. A crash before this point leaves the
	// name absent instead, which is what the reader is able to tell.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// artifact is one file an export writes: the name it takes in the output
// directory, and the bytes that go under it.
type artifact struct {
	name string
	body []byte
}

// writeFiles writes every artifact into dir, and removes the ones it had
// already written when one of them fails. An export that reports an error
// leaves nothing behind: a directory holding 39 of 500 statements and no index
// is one an ERP reading the directory bills 39 projects from, with every
// document in it well-formed and nothing saying that the other 461 are missing.
// The removals are best effort, because what the caller is told about is the
// write that failed.
//
// The names the renames created are on stable storage when this returns, so a
// caller that writes further files knows the ones before them are on the disk.
func writeFiles(dir string, artifacts []artifact) error {
	for i, a := range artifacts {
		if err := writeFile(dir, a.name, a.body); err != nil {
			remove(dir, artifacts[:i]...)
			return err
		}
	}
	if err := syncDir(dir); err != nil {
		remove(dir, artifacts...)
		return err
	}
	return nil
}

// writeIndexedFiles writes documents into dir and index, the file that names
// them, after them. The names of the documents are on stable storage before
// index is written, which is what makes "the index is written last" an ordering
// the disk holds rather than one the page cache does: a file whose bytes are
// synced is still nameless after a power loss until the directory entry that
// carries its name is synced as well, and renames of independent names are not
// ordered against each other in the writeback stream. With one sync at the end
// of everything, a node that came back could hold an index naming documents
// whose names never made it to the disk. An index that fails takes the
// documents with it, the way writeFiles takes the files before the one that
// failed.
func writeIndexedFiles(dir string, documents []artifact, index artifact) error {
	if err := writeFiles(dir, documents); err != nil {
		return err
	}
	if err := writeFiles(dir, []artifact{index}); err != nil {
		remove(dir, documents...)
		return err
	}
	return nil
}

// syncDir puts the names the renames in dir created on stable storage.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("writing the output directory %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("writing the output directory %s: %w", dir, err)
	}
	return nil
}

// remove takes back the artifacts an export had already written.
func remove(dir string, artifacts ...artifact) {
	for _, written := range artifacts {
		_ = os.Remove(filepath.Join(dir, written.name))
	}
}
