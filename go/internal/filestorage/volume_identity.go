package filestorage

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Identity belongs to the volume and must be included in backups. A cloned
// deployment retains it until operators deliberately provision a new volume.
func volumeIdentity(root string) (string, error) {
	lock, err := os.OpenFile(filepath.Join(root, ".gorge-volume-id.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer func() { _ = lock.Close() }()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
	path := filepath.Join(root, ".gorge-volume-id")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		raw = make([]byte, 16)
		if _, err = rand.Read(raw); err != nil {
			return "", err
		}
		id := hex.EncodeToString(raw)
		if err = durableWrite(path, []byte(id)); err != nil {
			return "", err
		}
		return id, nil
	}
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", errors.New("invalid volume identity")
	}
	if _, err = hex.DecodeString(string(raw)); err != nil {
		return "", errors.New("invalid volume identity")
	}
	return string(raw), nil
}
