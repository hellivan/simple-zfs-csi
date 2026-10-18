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
// lives now, or "" when it cannot be found. It never scans the pool: a promote
// only ever moves the snapshot to a clone of it, so the places are known.
//
//  1. The backing clone was cloned from the raw snapshot, so while the snapshot
//     is on another dataset the backing clone's origin names it (and follows it
//     through promotes of other clones).
//  2. Once the backing clone itself was promoted, the snapshot is on the backing
//     clone. Its origin may then be anything (the source's former origin), so
//     only the name decides.
//  3. Without a backing clone (interrupted create, manual change) the snapshot
//     has not left the source dataset.
//
// Every candidate is verified to exist.
func locateRawSnapshot(ctx context.Context, z zpool.ZFS, backingFull, recordedRaw, suffix string) (string, error) {
	exists := func(name string) (bool, error) {
		if _, err := z.Get(ctx, name, "type"); err != nil {
			if errors.Is(err, zpool.ErrNotExist) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	origin, err := z.Get(ctx, backingFull, "origin")
	switch {
	case err == nil:
		if origin = strings.TrimSpace(origin); strings.HasSuffix(origin, "@"+suffix) {
			if ok, err := exists(origin); err != nil || ok {
				return origin, err
			}
		}
	case !errors.Is(err, zpool.ErrNotExist):
		return "", err
	}
	for _, cand := range []string{backingFull + "@" + suffix, recordedRaw} {
		if ok, err := exists(cand); err != nil {
			return "", err
		} else if ok {
			return cand, nil
		}
	}
	return "", nil
}
