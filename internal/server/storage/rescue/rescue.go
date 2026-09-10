//go:build linux

package rescue

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	directory      = ".incus-rescue"
	imageDirectory = directory + "/image"
	statePath      = directory + "/state.json"
	temporaryRoot  = imageDirectory + "/rootfs"
)

var (
	tokenPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	imagePattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// State identifies the original and temporary roots across a process exit.
type State struct {
	Version  int    `json:"version"`
	Token    string `json:"token"`
	Image    string `json:"image"`
	Phase    string `json:"phase"`
	Original uint64 `json:"original"`
	Rescue   uint64 `json:"rescue"`
}

// ValidRequest validates the exact operation and cached image identities.
func ValidRequest(token string, image string) bool {
	return tokenPattern.MatchString(token) && imagePattern.MatchString(image)
}

// OriginalPath returns the retained root path inside its existing storage volume.
func OriginalPath(volumePath string) string {
	return filepath.Join(volumePath, "rootfs")
}

// ImagePath returns the temporary image metadata and template directory.
func ImagePath(volumePath string) string {
	return filepath.Join(volumePath, imageDirectory)
}

func inode(root *os.Root, path string) (uint64, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return 0, err
	}

	if !info.IsDir() {
		return 0, fmt.Errorf("Rescue path %q is not a directory", path)
	}

	return info.Sys().(*syscall.Stat_t).Ino, nil
}

func read(root *os.Root) (*State, error) {
	_, err := inode(root, directory)
	if err != nil {
		return nil, err
	}

	info, err := root.Lstat(statePath)
	if err != nil {
		return nil, err
	}

	if !info.Mode().IsRegular() || info.Size() > 4096 {
		return nil, errors.New("Invalid rescue transaction file")
	}

	data, err := root.ReadFile(statePath)
	if err != nil {
		return nil, err
	}

	var state State
	err = json.Unmarshal(data, &state)
	if err != nil {
		return nil, err
	}

	if state.Version != 1 || !ValidRequest(state.Token, state.Image) || state.Original == 0 {
		return nil, errors.New("Invalid rescue transaction identity")
	}

	switch state.Phase {
	case "preparing":
	case "restored":
	case "prepared", "active":
		if state.Rescue == 0 || state.Rescue == state.Original {
			return nil, errors.New("Invalid rescue directory identities")
		}

	default:
		return nil, errors.New("Invalid rescue transaction phase")
	}

	return &state, nil
}

func syncRoot(root *os.Root) error {
	file, err := root.Open(".")
	if err != nil {
		return err
	}

	defer func() { _ = file.Close() }()
	return unix.Syncfs(int(file.Fd()))
}

func save(root *os.Root, state *State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	err = root.WriteFile(statePath+".new", data, 0o600)
	if err != nil {
		return err
	}

	err = syncRoot(root)
	if err != nil {
		return err
	}

	err = root.Rename(statePath+".new", statePath)
	if err != nil {
		return err
	}

	return syncRoot(root)
}

func validateRoots(root *os.Root, state *State) error {
	original, err := inode(root, "rootfs")
	if err != nil {
		return err
	}

	temporary, err := inode(root, temporaryRoot)
	if err != nil {
		return err
	}

	if original != state.Original || temporary != state.Rescue {
		return errors.New("Rescue root directory identity changed")
	}

	return nil
}

// Prepare stages and activates a rescue image while retaining the original root.
func Prepare(volumePath string, token string, image string, unpack func(string) error) error {
	if !ValidRequest(token, image) {
		return errors.New("Invalid rescue operation or image identity")
	}

	root, err := os.OpenRoot(volumePath)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	state, err := read(root)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if state != nil && state.Phase == "restored" {
		_, err = root.Lstat(imageDirectory)
		if !errors.Is(err, fs.ErrNotExist) {
			return errors.New("Previous rescue cleanup has not completed")
		}

		// A completed receipt can survive a storage copy with different inodes.
		err = root.RemoveAll(directory)
		if err != nil {
			return err
		}

		state = nil
	}

	if state == nil {
		original, err := inode(root, "rootfs")
		if err != nil {
			return err
		}

		err = root.Mkdir(directory, 0o711)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}

		_, err = inode(root, directory)
		if err != nil {
			return err
		}

		state = &State{Version: 1, Token: token, Image: image, Phase: "preparing", Original: original}
		err = save(root, state)
		if err != nil {
			return err
		}
	}

	if state.Token != token || state.Image != image {
		return errors.New("Another rescue transaction owns this volume")
	}

	if state.Phase == "preparing" {
		original, err := inode(root, "rootfs")
		if err != nil || original != state.Original {
			return errors.New("Original root changed during rescue preparation")
		}

		err = root.RemoveAll(imageDirectory)
		if err != nil {
			return err
		}

		err = root.Mkdir(imageDirectory, 0o700)
		if err != nil {
			return err
		}

		err = unpack(ImagePath(volumePath))
		if err != nil {
			return err
		}

		state.Rescue, err = inode(root, temporaryRoot)
		if err != nil {
			return err
		}

		state.Phase = "prepared"
		err = save(root, state)
		if err != nil {
			return err
		}
	}

	err = validateRoots(root, state)
	if err != nil {
		return err
	}

	// The unprivileged LXC child must traverse the host-owned image parents.
	for _, path := range []string{directory, imageDirectory} {
		err = root.Chmod(path, 0o711)
		if err != nil {
			return err
		}
	}

	state.Phase = "active"
	return save(root, state)
}

// ValidateActive proves the retained and active roots still belong to this rescue.
func ValidateActive(volumePath string, token string) error {
	root, err := os.OpenRoot(volumePath)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	state, err := read(root)
	if err != nil {
		return err
	}

	if state.Token != token || state.Phase != "active" {
		return errors.New("Rescue root is not committed")
	}

	err = validateRoots(root, state)
	if err != nil {
		return err
	}

	return nil
}

// Restore removes the temporary root while preserving the canonical original root.
func Restore(volumePath string, token string, allowMissing bool) error {
	root, err := os.OpenRoot(volumePath)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()
	state, err := read(root)
	if errors.Is(err, fs.ErrNotExist) && allowMissing {
		_, imageErr := root.Lstat(imageDirectory)
		_, rootErr := inode(root, "rootfs")
		if errors.Is(imageErr, fs.ErrNotExist) && rootErr == nil {
			return nil
		}
	}

	if err != nil {
		return err
	}

	if state.Token != token {
		return errors.New("Another rescue transaction owns this volume")
	}

	if state.Phase == "preparing" || state.Phase == "restored" {
		original, err := inode(root, "rootfs")
		if err != nil || original != state.Original {
			return errors.New("Original root changed during rescue rollback")
		}
	} else {
		err = validateRoots(root, state)
		if err != nil {
			return err
		}
	}

	previousPhase := state.Phase
	// Keep the small receipt until the next rescue; deletion may lose its reply.
	state.Phase = "restored"
	err = save(root, state)
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		// Preserve committed root inodes so an interrupted cleanup remains verifiable.
		if previousPhase == "preparing" || previousPhase == "restored" {
			err = root.RemoveAll(imageDirectory)
		} else {
			err = discardTemporaryContents(root)
		}

		if err != nil {
			return err
		}

		err = save(root, state)
	}

	if err != nil {
		return err
	}

	err = root.RemoveAll(imageDirectory)
	if err != nil {
		return err
	}

	return syncRoot(root)
}

func discardTemporaryContents(root *os.Root) error {
	dir, err := root.Open(temporaryRoot)
	if err != nil {
		return err
	}

	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		err = root.RemoveAll(filepath.Join(temporaryRoot, entry.Name()))
		if err != nil {
			return err
		}
	}

	return syncRoot(root)
}
