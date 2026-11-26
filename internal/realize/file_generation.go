package realize

import "os"

// The change stamp distinguishes an in-place edit even if the editor restores
// both the bytes and mtime. The zero generation supplies no deletion proof.
type fileGeneration struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime"`
	Ctime  int64  `json:"ctime"`
}

func fileGenerationOf(info os.FileInfo) fileGeneration {
	device, inode, ok := filesystemIdentity(info)
	ctime := fileChangeTime(info)
	if !ok || inode == 0 || ctime == 0 {
		return fileGeneration{}
	}
	return fileGeneration{Device: device, Inode: inode, Size: info.Size(), Mtime: info.ModTime().UnixNano(), Ctime: ctime}
}
