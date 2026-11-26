// Copyright 2026 Yauhen Bichel
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
)

// Keeping the file small. A prune frees pages inside the file; SQLite hands them back to the disk only in
// auto_vacuum mode, and that mode is fixed when the first table is made. So: a file made by this version
// is INCREMENTAL and FreePages shrinks it after every prune; an older file keeps growing to its high-water
// mark until `llm-hops vacuum` (Vacuum) rebuilds it once, with the server stopped.

// freeStep is how many pages one write transaction of FreePages gives back (8 MB at the default page
// size), so senders wait for a step, not for the whole free list.
const freeStep = 2000

// Shrinks says whether the file gives pages back after a prune (auto_vacuum is INCREMENTAL).
func (s *Store) Shrinks() bool {
	var mode int
	_ = s.rdb.QueryRow("PRAGMA auto_vacuum").Scan(&mode)
	return mode == 2
}

// FreePages hands the pages a prune freed back to the disk and returns how many bytes that was. It does
// nothing on a file that does not shrink (see Shrinks).
func (s *Store) FreePages() (int64, error) {
	if !s.Shrinks() {
		return 0, nil
	}
	var pageSize, freed int64
	if err := s.rdb.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		return 0, err
	}
	for {
		s.mu.Lock()
		var before, after int64
		err := s.db.QueryRow("PRAGMA freelist_count").Scan(&before)
		if err == nil && before > 0 {
			if _, err = s.db.Exec(fmt.Sprintf("PRAGMA incremental_vacuum(%d)", freeStep)); err == nil {
				err = s.db.QueryRow("PRAGMA freelist_count").Scan(&after)
			}
		}
		s.mu.Unlock()
		if err != nil {
			return freed, err
		}
		freed += (before - after) * pageSize
		if after == 0 || after >= before {
			break
		}
	}
	if freed > 0 {
		s.mu.Lock()
		// the file itself gets shorter when the log is folded back into it; best effort, a reader may hold it
		_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		s.mu.Unlock()
	}
	return freed, nil
}

// SizeBytes is what the stored data takes: the pages in use, not the pages a prune left free.
func (s *Store) SizeBytes(ctx context.Context) (int64, error) {
	var pageSize, pages, free int64
	for q, into := range map[string]*int64{"PRAGMA page_size": &pageSize, "PRAGMA page_count": &pages, "PRAGMA freelist_count": &free} {
		if err := s.rdb.QueryRowContext(ctx, q).Scan(into); err != nil {
			return 0, err
		}
	}
	return (pages - free) * pageSize, nil
}

// PruneToSize rolls up and drops whole days, oldest first, until the data fits in maxBytes. It never
// touches the current UTC day (a day is never half rolled up), so `over` is true when today alone is larger.
func (s *Store) PruneToSize(maxBytes, nowMS int64) (pruned int64, over bool, err error) {
	ctx := context.Background()
	today := dayStart(nowMS)
	for {
		size, err := s.SizeBytes(ctx)
		if err != nil {
			return pruned, false, err
		}
		if size <= maxBytes {
			return pruned, false, nil
		}
		oldest, err := s.OldestTraceMS(ctx)
		if err != nil {
			return pruned, true, err
		}
		if oldest == 0 || oldest >= today {
			return pruned, true, nil
		}
		n, err := s.rollupBefore(min(dayStart(oldest)+86_400_000, today))
		if err != nil {
			return pruned, true, err
		}
		pruned += n
	}
}

// Vacuum rebuilds the file at path so that it gives pages back from then on (auto_vacuum INCREMENTAL), and
// returns its size before and after. It needs the file to itself (stop the server first) and free disk
// space of about the file's size while it runs.
func Vacuum(path string) (before, after int64, err error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	before = fi.Size()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return before, 0, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// a short wait, then fail: a running server holds the file and must be stopped, not waited for
	for _, q := range []string{"PRAGMA busy_timeout = 2000", "PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA auto_vacuum = INCREMENTAL", "VACUUM", "PRAGMA wal_checkpoint(TRUNCATE)"} {
		if _, err := db.Exec(q); err != nil {
			return before, 0, fmt.Errorf("%s: %w (is the server still running on this file?)", q, err)
		}
	}
	var mode int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil || mode != 2 {
		return before, 0, fmt.Errorf("auto_vacuum is %d after the rebuild, not incremental (%v)", mode, err)
	}
	if fi, err = os.Stat(path); err != nil {
		return before, 0, err
	}
	return before, fi.Size(), nil
}
