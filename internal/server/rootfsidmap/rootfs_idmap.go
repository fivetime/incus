// Package rootfsidmap records which ID map a rootfs was shifted with, so an
// interrupted shift can be recognised and finished after a restart.
//
// It lives outside internal/instance on purpose: that package is imported by
// the client, and shared/idmap pulls in cgo and libcap, which the client must
// not need in order to build.
package rootfsidmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"

	"github.com/google/uuid"

	"github.com/lxc/incus/v7/shared/idmap"
)

// RootfsIDMapProvenanceFilename is stored beside rootfs so it follows block-level snapshots without being guest-visible.
const RootfsIDMapProvenanceFilename = ".incus-idmap"

const (
	rootfsIDMapProvenanceVersion = 1
	rootfsIDMapStateStable       = "stable"
	rootfsIDMapStateTransition   = "transition"
	rootfsIDMapProvenanceMaxSize = 1024 * 1024
	rootfsIDMapIDSpaceSize       = int64(1 << 32)
)

type rootfsIDMapProvenance struct {
	Version int             `json:"version"`
	State   string          `json:"state"`
	IDMap   json.RawMessage `json:"idmap,omitempty"`
	From    json.RawMessage `json:"from,omitempty"`
	To      json.RawMessage `json:"to,omitempty"`
}

// RootfsIDMapApply applies an idmap transition to an already mounted rootfs.
type RootfsIDMapApply func(from *idmap.Set, to *idmap.Set) error

// RootfsIDMapVolatileSet mirrors the durable map into volatile.last_state.idmap.
type RootfsIDMapVolatileSet func(idmapJSON string) error

func rootfsIDMapJSON(idmapSet *idmap.Set) (json.RawMessage, string, error) {
	idmapJSON, err := idmapSet.ToJSON()
	if err != nil {
		return nil, "", err
	}

	return json.RawMessage(idmapJSON), idmapJSON, nil
}

func validateRootfsIDMap(idmapSet *idmap.Set) error {
	if idmapSet == nil {
		return nil
	}

	for i, entry := range idmapSet.Entries {
		if !entry.IsUID && !entry.IsGID {
			return fmt.Errorf("ID map entry %d maps neither UIDs nor GIDs", i)
		}

		if entry.HostID < 0 || entry.NSID < 0 || entry.MapRange <= 0 {
			return fmt.Errorf("ID map entry %d contains an invalid range", i)
		}

		if entry.HostID > rootfsIDMapIDSpaceSize-entry.MapRange || entry.NSID > rootfsIDMapIDSpaceSize-entry.MapRange {
			return fmt.Errorf("ID map entry %d exceeds the 32-bit UID/GID space", i)
		}

		for j := 0; j < i; j++ {
			other := idmapSet.Entries[j]
			sameKind := (entry.IsUID && other.IsUID) || (entry.IsGID && other.IsGID)
			if !sameKind {
				continue
			}

			if entry.HostID < other.HostID+other.MapRange && other.HostID < entry.HostID+entry.MapRange {
				return fmt.Errorf("ID map entries %d and %d contain overlapping host ID ranges", j, i)
			}

			if entry.NSID < other.NSID+other.MapRange && other.NSID < entry.NSID+entry.MapRange {
				return fmt.Errorf("ID map entries %d and %d contain overlapping namespace ID ranges", j, i)
			}
		}
	}

	return nil
}

func rootfsIDMapFromJSON(data json.RawMessage) (*idmap.Set, error) {
	if len(data) == 0 {
		return nil, errors.New("Missing ID map")
	}

	idmapSet, err := idmap.NewSetFromJSON(string(data))
	if err != nil {
		return nil, err
	}

	err = validateRootfsIDMap(idmapSet)
	if err != nil {
		return nil, err
	}

	return idmapSet, nil
}

func readRootfsIDMapProvenance(volumePath string) (*rootfsIDMapProvenance, bool, error) {
	root, err := os.OpenRoot(volumePath)
	if err != nil {
		return nil, false, err
	}

	defer func() { _ = root.Close() }()

	info, err := root.Lstat(RootfsIDMapProvenanceFilename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, err
	}

	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("Rootfs ID map provenance %q is not a regular file", RootfsIDMapProvenanceFilename)
	}

	f, err := root.Open(RootfsIDMapProvenanceFilename)
	if err != nil {
		return nil, false, err
	}

	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, rootfsIDMapProvenanceMaxSize+1))
	if err != nil {
		return nil, false, err
	}

	if len(data) > rootfsIDMapProvenanceMaxSize {
		return nil, false, errors.New("Rootfs ID map provenance is too large")
	}

	var provenance rootfsIDMapProvenance
	err = json.Unmarshal(data, &provenance)
	if err != nil {
		return nil, false, fmt.Errorf("Invalid rootfs ID map provenance: %w", err)
	}

	if provenance.Version != rootfsIDMapProvenanceVersion {
		return nil, false, fmt.Errorf("Unsupported rootfs ID map provenance version %d", provenance.Version)
	}

	switch provenance.State {
	case rootfsIDMapStateStable:
		_, err = rootfsIDMapFromJSON(provenance.IDMap)
	case rootfsIDMapStateTransition:
		var from *idmap.Set
		from, err = rootfsIDMapFromJSON(provenance.From)
		if err == nil {
			var to *idmap.Set
			to, err = rootfsIDMapFromJSON(provenance.To)
			if err == nil {
				err = rootfsIDMapTransitionSafe(from, to)
			}
		}
	default:
		err = fmt.Errorf("Invalid rootfs ID map provenance state %q", provenance.State)
	}

	if err != nil {
		return nil, false, err
	}

	return &provenance, true, nil
}

func writeRootfsIDMapProvenance(volumePath string, provenance rootfsIDMapProvenance) error {
	data, err := json.Marshal(provenance)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(volumePath)
	if err != nil {
		return err
	}

	defer func() { _ = root.Close() }()

	tempName := fmt.Sprintf("%s.tmp.%s", RootfsIDMapProvenanceFilename, uuid.New().String())
	f, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	removeTemp := true
	defer func() {
		if removeTemp {
			_ = root.Remove(tempName)
		}
	}()

	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}

	closeErr := f.Close()
	if err != nil {
		return err
	}

	if closeErr != nil {
		return closeErr
	}

	err = root.Rename(tempName, RootfsIDMapProvenanceFilename)
	if err != nil {
		return err
	}

	removeTemp = false

	dir, err := root.Open(".")
	if err != nil {
		return err
	}

	err = dir.Sync()
	closeErr = dir.Close()
	if err != nil && runtime.GOOS != "windows" {
		return err
	}

	return closeErr
}

func writeStableRootfsIDMapProvenance(volumePath string, idmapSet *idmap.Set) error {
	idmapData, _, err := rootfsIDMapJSON(idmapSet)
	if err != nil {
		return err
	}

	return writeRootfsIDMapProvenance(volumePath, rootfsIDMapProvenance{
		Version: rootfsIDMapProvenanceVersion,
		State:   rootfsIDMapStateStable,
		IDMap:   idmapData,
	})
}

func writeTransitionRootfsIDMapProvenance(volumePath string, from *idmap.Set, to *idmap.Set) error {
	fromData, _, err := rootfsIDMapJSON(from)
	if err != nil {
		return err
	}

	toData, _, err := rootfsIDMapJSON(to)
	if err != nil {
		return err
	}

	return writeRootfsIDMapProvenance(volumePath, rootfsIDMapProvenance{
		Version: rootfsIDMapProvenanceVersion,
		State:   rootfsIDMapStateTransition,
		From:    fromData,
		To:      toData,
	})
}

func rootfsIDMapHostRangesOverlap(a *idmap.Set, b *idmap.Set) bool {
	if a == nil || b == nil {
		return false
	}

	for _, entryA := range a.Entries {
		for _, entryB := range b.Entries {
			sameKind := (entryA.IsUID && entryB.IsUID) || (entryA.IsGID && entryB.IsGID)
			if !sameKind {
				continue
			}

			if entryA.HostID < entryB.HostID+entryB.MapRange && entryB.HostID < entryA.HostID+entryA.MapRange {
				return true
			}
		}
	}

	return false
}

func rootfsIDMapHostNamespaceRangesOverlap(hostMaps []*idmap.Set, namespaceMaps []*idmap.Set) bool {
	for _, hostMap := range hostMaps {
		if hostMap == nil {
			continue
		}

		for _, hostEntry := range hostMap.Entries {
			if hostEntry.HostID == hostEntry.NSID {
				continue
			}

			for _, namespaceMap := range namespaceMaps {
				if namespaceMap == nil {
					continue
				}

				for _, namespaceEntry := range namespaceMap.Entries {
					sameKind := (hostEntry.IsUID && namespaceEntry.IsUID) || (hostEntry.IsGID && namespaceEntry.IsGID)
					if !sameKind {
						continue
					}

					if hostEntry.HostID < namespaceEntry.NSID+namespaceEntry.MapRange && namespaceEntry.NSID < hostEntry.HostID+hostEntry.MapRange {
						return true
					}
				}
			}
		}
	}

	return false
}

func rootfsIDMapTransitionSafe(from *idmap.Set, to *idmap.Set) error {
	err := validateRootfsIDMap(from)
	if err != nil {
		return err
	}

	err = validateRootfsIDMap(to)
	if err != nil {
		return err
	}

	if from.Equals(to) {
		return nil
	}

	if rootfsIDMapHostRangesOverlap(from, to) {
		return errors.New("Rootfs ID map transition contains overlapping host ID ranges")
	}

	if rootfsIDMapHostNamespaceRangesOverlap([]*idmap.Set{from, to}, []*idmap.Set{from, to}) {
		return errors.New("Rootfs ID map transition host ranges overlap namespace ranges")
	}

	return nil
}

// SeedNormalizedRootfsIDMapProvenance records a trusted newly created root as namespace-owned.
func SeedNormalizedRootfsIDMapProvenance(volumePath string) error {
	_, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		return err
	}

	if exists {
		return ValidateNormalizedRootfsIDMapProvenance(volumePath)
	}

	return writeStableRootfsIDMapProvenance(volumePath, nil)
}

// ValidateRootfsIDMapProvenance verifies that a claimed root carries a recoverable journal.
func ValidateRootfsIDMapProvenance(volumePath string) error {
	_, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		return err
	}

	if !exists {
		return errors.New("Rootfs ID map provenance is missing")
	}

	return nil
}

// ValidateNormalizedRootfsIDMapProvenance verifies that a root is durably recorded as namespace-owned.
func ValidateNormalizedRootfsIDMapProvenance(volumePath string) error {
	provenance, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		return err
	}

	if !exists {
		return errors.New("Rootfs ID map provenance is missing")
	}

	if provenance.State != rootfsIDMapStateStable {
		return errors.New("Rootfs ID map provenance is not in a stable state")
	}

	stable, err := rootfsIDMapFromJSON(provenance.IDMap)
	if err != nil {
		return err
	}

	if stable != nil {
		return errors.New("Rootfs ID map provenance is not namespace-owned")
	}

	return nil
}

// RecoverRootfsIDMapProvenance seeds a legacy volume or completes an interrupted idmap transition.
func RecoverRootfsIDMapProvenance(volumePath string, legacy *idmap.Set, apply RootfsIDMapApply, syncFS func() error, volatileSet RootfsIDMapVolatileSet) (*idmap.Set, error) {
	provenance, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		return nil, err
	}

	if !exists {
		if legacy == nil {
			return nil, errors.New("Rootfs ID map provenance is missing and no legacy on-disk ID map is available")
		}

		err = validateRootfsIDMap(legacy)
		if err != nil {
			return nil, err
		}

		err = writeStableRootfsIDMapProvenance(volumePath, legacy)
		if err != nil {
			return nil, fmt.Errorf("Seed rootfs ID map provenance: %w", err)
		}

		_, legacyJSON, err := rootfsIDMapJSON(legacy)
		if err != nil {
			return nil, err
		}

		err = volatileSet(legacyJSON)
		if err != nil {
			return nil, fmt.Errorf("Mirror rootfs ID map provenance: %w", err)
		}

		return legacy, nil
	}

	if provenance.State == rootfsIDMapStateStable {
		stable, err := rootfsIDMapFromJSON(provenance.IDMap)
		if err != nil {
			return nil, err
		}

		_, stableJSON, err := rootfsIDMapJSON(stable)
		if err != nil {
			return nil, err
		}

		err = volatileSet(stableJSON)
		if err != nil {
			return nil, fmt.Errorf("Mirror rootfs ID map provenance: %w", err)
		}

		return stable, nil
	}

	from, err := rootfsIDMapFromJSON(provenance.From)
	if err != nil {
		return nil, err
	}

	to, err := rootfsIDMapFromJSON(provenance.To)
	if err != nil {
		return nil, err
	}

	err = apply(from, to)
	if err != nil {
		return nil, fmt.Errorf("Recover rootfs ID map transition: %w", err)
	}

	err = syncFS()
	if err != nil {
		return nil, fmt.Errorf("Sync recovered rootfs ID map transition: %w", err)
	}

	err = writeStableRootfsIDMapProvenance(volumePath, to)
	if err != nil {
		return nil, fmt.Errorf("Commit recovered rootfs ID map transition: %w", err)
	}

	_, toJSON, err := rootfsIDMapJSON(to)
	if err != nil {
		return nil, err
	}

	err = volatileSet(toJSON)
	if err != nil {
		return nil, fmt.Errorf("Mirror recovered rootfs ID map provenance: %w", err)
	}

	return to, nil
}

// TransitionRootfsIDMapProvenance durably journals and applies an idmap change.
func TransitionRootfsIDMapProvenance(volumePath string, from *idmap.Set, to *idmap.Set, apply RootfsIDMapApply, syncFS func() error, volatileSet RootfsIDMapVolatileSet) error {
	if from.Equals(to) {
		_, toJSON, err := rootfsIDMapJSON(to)
		if err != nil {
			return err
		}

		return volatileSet(toJSON)
	}

	err := rootfsIDMapTransitionSafe(from, to)
	if err != nil {
		return err
	}

	provenance, exists, err := readRootfsIDMapProvenance(volumePath)
	if err != nil {
		return err
	}

	if !exists || provenance.State != rootfsIDMapStateStable {
		return errors.New("Rootfs ID map provenance is not in a stable state")
	}

	stable, err := rootfsIDMapFromJSON(provenance.IDMap)
	if err != nil {
		return err
	}

	if !stable.Equals(from) {
		return errors.New("Rootfs ID map provenance changed before transition")
	}

	err = writeTransitionRootfsIDMapProvenance(volumePath, from, to)
	if err != nil {
		return fmt.Errorf("Journal rootfs ID map transition: %w", err)
	}

	err = apply(from, to)
	if err != nil {
		return fmt.Errorf("Apply rootfs ID map transition: %w", err)
	}

	err = syncFS()
	if err != nil {
		return fmt.Errorf("Sync rootfs ID map transition: %w", err)
	}

	err = writeStableRootfsIDMapProvenance(volumePath, to)
	if err != nil {
		return fmt.Errorf("Commit rootfs ID map transition: %w", err)
	}

	_, toJSON, err := rootfsIDMapJSON(to)
	if err != nil {
		return err
	}

	err = volatileSet(toJSON)
	if err != nil {
		return fmt.Errorf("Mirror rootfs ID map provenance: %w", err)
	}

	return nil
}
