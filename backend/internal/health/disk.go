package health

import "syscall"

// DiskUsageFunc reports total and available bytes for the filesystem backing
// path. Injectable so the disk component can be unit-tested without touching a
// real filesystem; the default is defaultDiskUsage over syscall.Statfs.
type DiskUsageFunc func(path string) (totalBytes, availBytes uint64, err error)

// defaultDiskUsage statfs's path and returns total + unprivileged-available
// bytes.
//
// Coverage note: in prod the backend container has no volume mounts, so
// statting the configured path (default "/") measures the container's root
// overlay, whose backing store is the Pi's root SD-card filesystem — the SAME
// filesystem that backs the crm-postgres named volume under rootless podman's
// graphroot. So the floor protects the whole SD card, which is what actually
// fills up and kills Postgres. It is NOT a direct stat of the Postgres volume
// mount and would not see a hypothetical separate data disk (none exists
// today; re-point HEALTH_DISK_PATH if the storage topology ever changes).
//
// Bavail (df's unprivileged-available) is the numerator, so free_percent reads
// a few points lower than (100 − df Use%) on ext4 because df's denominator
// excludes the root reserve. That is fine for a floor check — this does not
// claim df equivalence.
//
// Statfs_t.Bsize is int64 on linux and uint32 on darwin; uint64(st.Bsize)
// compiles on both (CI is linux, dev is darwin — the only platforms that build
// this repo), so no build tags are needed.
func defaultDiskUsage(path string) (totalBytes, availBytes uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	totalBytes = st.Blocks * bsize
	availBytes = st.Bavail * bsize
	return totalBytes, availBytes, nil
}
