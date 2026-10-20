package collect

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"kubemap/pkg/protocol"
)

// skipFSType lists filesystem types that hold no disk data. Network filesystems
// are skipped too: statfs on a dead NFS server can hang the collector.
var skipFSType = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "ramfs": true, "overlay": true, "squashfs": true,
	"proc": true, "sysfs": true, "cgroup": true, "cgroup2": true, "devpts": true,
	"mqueue": true, "debugfs": true, "tracefs": true, "securityfs": true,
	"pstore": true, "bpf": true, "configfs": true, "fusectl": true,
	"hugetlbfs": true, "autofs": true, "binfmt_misc": true, "nsfs": true,
	"rpc_pipefs": true, "efivarfs": true, "selinuxfs": true, "fuse.lxcfs": true,
	"nfs": true, "nfs4": true, "cifs": true, "smb3": true, "ceph": true,
	"glusterfs": true, "fuse.sshfs": true,
}

type mount struct {
	dev        string // major:minor
	mountPoint string
	fsType     string
	source     string
}

// parseMountinfo reads /proc/<pid>/mountinfo:
// "29 1 252:0 / / rw,relatime shared:1 - ext4 /dev/mapper/vg-root rw".
func parseMountinfo(path string) ([]mount, error) {
	var out []mount
	err := scanLines(path, func(line string) error {
		f := strings.Fields(line)
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(f) {
			return nil
		}
		out = append(out, mount{dev: f[2], mountPoint: unescape(f[4]), fsType: f[sep+1], source: f[sep+2]})
		return nil
	})
	return out, err
}

// unescape decodes the octal escapes (\040 for space) used in mountinfo.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// realMounts keeps one mount per block device: the one with the shortest
// path, so "/" wins over the bind mounts of the same disk that kubelet and
// containerd create for every pod.
func realMounts(all []mount) []mount {
	sort.SliceStable(all, func(i, j int) bool { return len(all[i].mountPoint) < len(all[j].mountPoint) })
	seen := map[string]bool{}
	var out []mount
	for _, m := range all {
		if skipFSType[m.fsType] || seen[m.dev] {
			continue
		}
		seen[m.dev] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mountPoint < out[j].mountPoint })
	return out
}

type fsUsage struct {
	total, free, avail      uint64 // bytes
	inodesTotal, inodesFree uint64
}

// filesystems reports each real filesystem of the host. Mounts come from
// PID 1 (the host's mount namespace when the pod has hostPID) and are read
// through HostRoot. Filesystems that fail statfs or have size 0 are skipped.
//
// The host mounts are recursively read-only with no mount propagation
// (review-km-44 L5), so a disk mounted on the host after the pod started is
// not under HostRoot: the path there is a plain directory of the parent
// filesystem, and statfs would report the parent's numbers under the new
// disk's name. The device of the path is checked against the mount's, and
// such a mount is left out (and logged once) until the pod restarts.
func (c *Collector) filesystems() []protocol.FilesystemStats {
	mounts, err := parseMountinfo(c.proc("1/mountinfo"))
	if err != nil {
		if mounts, err = parseMountinfo(c.proc("self/mountinfo")); err != nil {
			return nil
		}
	}
	var out []protocol.FilesystemStats
	for _, m := range realMounts(mounts) {
		path := filepath.Join(c.cfg.HostRoot, m.mountPoint)
		if dev, err := c.devOf(path); err == nil && dev != m.dev {
			if !c.unseen[m.mountPoint] {
				c.unseen[m.mountPoint] = true
				slog.Warn("filesystem not measured: it was mounted after the node agent started; restart the node agent pod to include it",
					"mount", m.mountPoint, "device", m.source, "want", m.dev, "seen", dev)
			}
			continue
		}
		delete(c.unseen, m.mountPoint)
		u, err := c.statfs(path)
		if err != nil || u.total == 0 {
			continue
		}
		out = append(out, protocol.FilesystemStats{
			MountPoint: m.mountPoint, Device: m.source, FSType: m.fsType,
			TotalBytes: u.total, UsedBytes: u.total - min(u.free, u.total), AvailableBytes: u.avail,
			InodesTotal: u.inodesTotal, InodesFree: u.inodesFree,
		})
	}
	return out
}
