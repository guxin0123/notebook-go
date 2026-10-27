package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var (
	errInvalidNoteID = errors.New("invalid note id")
	errNoteTooLarge  = errors.New("note too large")
)

// IDs are used as file names, so keep the character set deliberately small.
var noteIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validNoteID(id string) bool {
	return noteIDPattern.MatchString(id)
}

type noteStore struct {
	dir      string
	maxBytes int64

	// Saving is serialised per note without keeping an unbounded map of locks.
	locks [64]sync.Mutex
}

func newNoteStore(dir string, maxBytes int64) (*noteStore, error) {
	if dir == "" {
		return nil, errors.New("data directory must not be empty")
	}
	if maxBytes < 1 {
		return nil, errors.New("max bytes must be positive")
	}

	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("data path is not a directory")
	}

	// Fail at startup rather than after the first autosave.
	probe, err := os.CreateTemp(dir, ".notebook-write-test-*")
	if err != nil {
		return nil, err
	}
	probeName := probe.Name()
	_ = probe.Close()
	if err := os.Remove(probeName); err != nil {
		return nil, err
	}

	return &noteStore{
		dir:      dir,
		maxBytes: maxBytes,
	}, nil
}

func (s *noteStore) notePath(id string) string {
	return filepath.Join(s.dir, id+".txt")
}

func (s *noteStore) lockFor(id string) *sync.Mutex {
	// FNV-1a, folded into a fixed number of locks.
	var hash uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		hash ^= uint32(id[i])
		hash *= 16777619
	}
	return &s.locks[hash%uint32(len(s.locks))]
}

func (s *noteStore) exists(id string) (bool, error) {
	if !validNoteID(id) {
		return false, errInvalidNoteID
	}

	_, err := os.Stat(s.notePath(id))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// load returns the note contents and whether the note file exists. A note that
// has not been saved yet is not an error; it is represented as an empty editor.
func (s *noteStore) load(id string) ([]byte, bool, error) {
	if !validNoteID(id) {
		return nil, false, errInvalidNoteID
	}

	f, err := os.Open(s.notePath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("note path is not a regular file")
	}
	if info.Size() > s.maxBytes {
		return nil, true, errNoteTooLarge
	}

	data, err := io.ReadAll(io.LimitReader(f, s.maxBytes+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(data)) > s.maxBytes {
		return nil, true, errNoteTooLarge
	}
	return data, true, nil
}

// save writes through a temporary file and renames it into place, so readers
// never observe a partially written note and a crash cannot truncate the note.
func (s *noteStore) save(id string, data []byte) error {
	if !validNoteID(id) {
		return errInvalidNoteID
	}
	if int64(len(data)) > s.maxBytes {
		return errNoteTooLarge
	}

	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()

	tmp, err := os.CreateTemp(s.dir, ".notebook-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpName, s.notePath(id)); err != nil {
		return err
	}
	removeTemp = false

	// Make the rename durable when the platform supports syncing a directory.
	if d, err := os.Open(s.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
