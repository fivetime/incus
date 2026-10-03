// Package cephownership transfers an existing RBD ownership marker by immutable identity.
package cephownership

import (
	"errors"
	"fmt"
	"strings"
)

// Binding identifies the cluster, pool and image without resolving an image name.
type Binding struct {
	Cluster    string
	User       string
	ConfigFile string
	FSID       string
	PoolID     int64
	ImageID    string
}

func (b Binding) validate(previous string, next string) error {
	if b.Cluster == "" || b.User == "" || b.PoolID < 0 {
		return errors.New("Incomplete Ceph ownership binding")
	}

	if len(b.FSID) != 36 || b.FSID != strings.ToLower(b.FSID) {
		return errors.New("Invalid Ceph cluster FSID")
	}

	for i, c := range b.FSID {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return errors.New("Invalid Ceph cluster FSID")
			}
		} else if !strings.ContainsRune("0123456789abcdef", c) {
			return errors.New("Invalid Ceph cluster FSID")
		}
	}

	if b.ImageID == "" || strings.Trim(b.ImageID, "0123456789abcdef") != "" {
		return errors.New("Invalid RBD image ID")
	}

	for _, marker := range []string{previous, next} {
		if len(marker) != 71 || !strings.HasPrefix(marker, "sha256:") || strings.Trim(marker[7:], "0123456789abcdef") != "" {
			return errors.New("Invalid RBD ownership marker")
		}
	}

	for _, value := range []string{b.Cluster, b.User, b.ConfigFile} {
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("Ceph connection binding contains a NUL")
		}
	}

	return nil
}
