package history

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"

	"ttc/internal/filelock"
)

// readSnapshot owns a separate read-only WAL connection, so decoding or a slow
// output writer cannot occupy the Store's serialized commit connection. Its
// shared lifetime lock fences schema upgrades and VACUUM while the reader lives.
// Runtime activity protection, not this lock, retains the loaded lineage's files.
func (s *Store) readSnapshot(ctx context.Context) (contextHistoryReader, func(), error) {
	lock, err := filelock.AcquireShared(ctx, filepath.Join(s.Root, "history-use.lock"))
	if err != nil {
		return contextHistoryReader{}, nil, err
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(s.Root, "history.sqlite"), RawQuery: "mode=ro&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		lock.Close()
		return contextHistoryReader{}, nil, err
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		lock.Close()
		return contextHistoryReader{}, nil, err
	}
	return contextHistoryReader{ctx, tx}, func() { tx.Rollback(); db.Close(); lock.Close() }, nil
}
