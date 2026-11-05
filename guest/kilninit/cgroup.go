//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	// userCgroup is the subtree the template user owns, the way systemd
	// delegates a user slice. Its processes start in userLeaf, because a
	// cgroup that holds processes cannot hand controllers to its children.
	userCgroup = cgroupRoot + "/user"
	userLeaf   = userCgroup + "/init"
)

// cgroupControllers are the controllers kilninit hands down to the template
// user. A kernel without one of them simply does not offer it.
var cgroupControllers = []string{"cpu", "memory", "pids", "io"}

// cgroupFD is the open userLeaf, which every child of the template user
// starts in. It is opened once and kept, because the path never changes.
var cgroupFD = -1

// mountCgroup mounts cgroup2 at /sys/fs/cgroup and enables the controllers
// for its children. It does nothing when cgroup2 is already there.
func mountCgroup() error {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &st); err == nil && st.Type == unix.CGROUP2_SUPER_MAGIC {
		return nil
	}
	if err := unix.Mount("cgroup2", cgroupRoot, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "nsdelegate"); err != nil {
		return fmt.Errorf("%s: %w", cgroupRoot, err)
	}
	return enableControllers(cgroupRoot)
}

// enableControllers turns on, for the children of dir, every controller in
// cgroupControllers that dir has.
func enableControllers(dir string) error {
	have, err := os.ReadFile(dir + "/cgroup.controllers")
	if err != nil {
		return err
	}
	offered := map[string]bool{}
	for _, c := range strings.Fields(string(have)) {
		offered[c] = true
	}
	var want []string
	for _, c := range cgroupControllers {
		if offered[c] {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if err := os.WriteFile(dir+"/cgroup.subtree_control", []byte(strings.Join(want, " ")), 0); err != nil {
		return fmt.Errorf("%s controllers: %w", dir, err)
	}
	return nil
}

// delegateCgroup gives the template user its own cgroup subtree and returns
// the leaf its processes start in. kilninit sets the subtree up only when it
// creates it. After that the subtree belongs to the user, and a fork or a wake
// keeps whatever the user made of it. The caller holds resumeMu.
func delegateCgroup(u *runAs) (int, error) {
	if cgroupFD >= 0 {
		return cgroupFD, nil
	}
	err := os.Mkdir(userCgroup, 0o755)
	created := err == nil
	if err != nil && !errors.Is(err, os.ErrExist) {
		return -1, err
	}
	if created {
		if err := enableControllers(userCgroup); err != nil {
			return -1, err
		}
	}
	if err := os.Mkdir(userLeaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return -1, err
	}
	if created {
		for _, dir := range []string{userCgroup, userLeaf} {
			for _, name := range []string{"", "/cgroup.procs", "/cgroup.subtree_control", "/cgroup.threads"} {
				if err := os.Chown(dir+name, int(u.uid), int(u.gid)); err != nil {
					return -1, err
				}
			}
		}
	}
	fd, err := unix.Open(userLeaf, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s: %w", userLeaf, err)
	}
	cgroupFD = fd
	return fd, nil
}
