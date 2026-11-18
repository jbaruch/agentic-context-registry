package realizeapp

import (
	"os"
	"syscall"
)

func appliedMetadata(info os.FileInfo) (appliedFile, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return appliedFile{}, false
	}
	return appliedFile{
		Device: uint64(stat.Dev), Inode: stat.Ino, Size: stat.Size,
		ModSeconds: stat.Mtimespec.Sec, ModNanos: stat.Mtimespec.Nsec,
		ChangeSeconds: stat.Ctimespec.Sec, ChangeNanos: stat.Ctimespec.Nsec,
	}, true
}
