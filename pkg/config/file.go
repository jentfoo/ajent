package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
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

// WriteFileAtomic writes data through a temp file in the same directory, so a
// crash mid-write cannot leave a partial file behind.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
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
	} else if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
