package controller

import (
	"context"
	"errors"
	"path"
	"strings"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// backingCloneFullName is the full ZFS name of a snapshot's backing clone: a
// flat sibling of the source dataset named after the snapshot's short name.
func backingCloneFullName(poolName, sourceDataset, snapshotName string) string {
	return poolName + "/" + path.Join(path.Dir(strings.Trim(sourceDataset, "/")), snapshotName)
}

// locateRawSnapshot returns where the raw snapshot with the given short name
// lives now, or "" when it cannot be found. A promote moves a snapshot between
// datasets, so no fixed address is assumed:
//
//  1. The backing clone was cloned from the raw snapshot, so its origin names the
//     snapshot as long as the backing clone itself has not been promoted. One
//     property read, verified against the snapshot's existence.
//  2. Otherwise (backing clone missing or promoted, origin empty or stale) the
//     pool-wide scan finds it by short name, which is unique.
func locateRawSnapshot(ctx context.Context, z zpool.ZFS, poolName, backingFull, suffix string) (string, error) {
	origin, err := z.Get(ctx, backingFull, "origin")
	switch {
	case err == nil:
		if origin = strings.TrimSpace(origin); origin != "" && origin != "-" && strings.HasSuffix(origin, "@"+suffix) {
			if _, gerr := z.Get(ctx, origin, "type"); gerr == nil {
				return origin, nil
			} else if !errors.Is(gerr, zpool.ErrNotExist) {
				return "", gerr
			}
		}
	case !errors.Is(err, zpool.ErrNotExist):
		return "", err
	}
	return z.FindSnapshot(ctx, poolName, suffix)
}
