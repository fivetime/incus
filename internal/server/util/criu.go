package util

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// CRIUSHA256 fingerprints the executable resolved in the daemon's environment.
func CRIUSHA256() (string, error) {
	path, err := exec.LookPath("criu")
	if err != nil {
		return "", err
	}

	return executableSHA256(path)
}

func executableSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}

	defer file.Close()

	before, err := file.Stat()
	if err != nil {
		return "", err
	}

	const maximumSize = 128 * 1024 * 1024
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 || before.Size() > maximumSize {
		return "", fmt.Errorf("Invalid CRIU executable")
	}

	digest := sha256.New()
	count, err := io.Copy(digest, io.LimitReader(file, maximumSize+1))
	if err != nil {
		return "", err
	}

	after, err := file.Stat()
	if err != nil {
		return "", err
	}

	current, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	if count != before.Size() || !os.SameFile(before, after) || !os.SameFile(after, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", fmt.Errorf("CRIU executable changed during observation")
	}

	return hex.EncodeToString(digest.Sum(nil)), nil
}
