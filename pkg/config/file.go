package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/jentfoo/ajent/pkg/paths"
)

// SecretPerm is the mode a file holding credentials should carry.
const SecretPerm = 0o600

// CheckSecretPerms returns a warning when path is group or world accessible, and
// empty otherwise. Use it on any file that may hold an API key.
func CheckSecretPerms(path string) string {
	if runtime.GOOS == "windows" {
		return "" // mode bits are not meaningful
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	mode := fi.Mode().Perm()
	if mode&0o077 == 0 {
		return ""
	}
	return path + " is readable by other users (mode " +
		strconv.FormatUint(uint64(mode), 8) + "), it may hold an API key; chmod 600 it"
}

// ReadForSave reads the file a writer is about to replace, with hand-edit
// artifacts relaxed by RelaxJSONReport. A missing file reads as empty.
// droppedComments marks // comments the rewrite will drop; writers surface it
// only once the write lands, so a refusal never claims a loss.
func ReadForSave(path string) (raw []byte, droppedComments bool, err error) {
	raw, err = os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil // a brand new file starts empty
	} else if err != nil {
		return nil, false, err
	}
	// a trailing comma is fixed silently; comments are the only content lost
	raw, droppedComments = RelaxJSONReport(raw)
	return raw, droppedComments, nil
}

// WriteFileAtomic writes data to path with perm atomically, so readers see
// either the old or the new content and a crash mid-write never leaves a
// partial file. Missing parent directories are created.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, paths.DirPerm); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op once the rename succeeds

	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	} else if err = f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	} else if err = f.Sync(); err != nil { // durability across an OS crash, not just a process one
		_ = f.Close()
		return err
	} else if err = f.Close(); err != nil {
		return err
	} else if err = os.Rename(tmp, path); err != nil {
		return err
	}
	// the rename already committed; a failed directory fsync only weakens crash
	// durability, and reporting it would make callers treat the write as undone
	_ = syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename into it survives a power loss.
// Windows cannot fsync directory handles, so it is a no-op there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err = d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
