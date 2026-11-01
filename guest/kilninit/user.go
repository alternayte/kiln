//go:build linux

package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/alternayte/kiln/internal/guestproto"
	"golang.org/x/sys/unix"
)

const (
	passwdPath = "/etc/passwd"
	groupPath  = "/etc/group"
)

// runAs is the template user. Everything a caller asks for runs as this user,
// and a template with no user runs as root.
type runAs struct {
	name   string
	uid    uint32
	gid    uint32
	groups []uint32
	home   string
}

// templateUser is set by the resume hook on every restore.
var templateUser *runAs

// currentUser returns the template user, or nil for root.
func currentUser() *runAs {
	resumeMu.Lock()
	defer resumeMu.Unlock()
	return templateUser
}

// lookupUser finds a user by name or numeric id in the image's own files. A
// user who is not there is an error, because a uid alone gives a process no
// home and no groups.
func lookupUser(spec, passwd, group string) (*runAs, error) {
	f, err := os.Open(passwd)
	if err != nil {
		return nil, fmt.Errorf("user %q: %w", spec, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || (fields[0] != spec && fields[2] != spec) {
			continue
		}
		uid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("user %q: uid %q: %w", spec, fields[2], err)
		}
		gid, err := strconv.ParseUint(fields[3], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("user %q: gid %q: %w", spec, fields[3], err)
		}
		u := &runAs{name: fields[0], uid: uint32(uid), gid: uint32(gid), home: fields[5]}
		u.groups = memberGroups(group, u.name, u.gid)
		return u, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("user %q: %w", spec, err)
	}
	return nil, fmt.Errorf("the image has no user %q", spec)
}

// memberGroups returns the primary group and every group that names the user.
// An image with no group file still has the primary group.
func memberGroups(path, name string, primary uint32) []uint32 {
	groups := []uint32{primary}
	f, err := os.Open(path)
	if err != nil {
		return groups
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 4 {
			continue
		}
		gid, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil || uint32(gid) == primary {
			continue
		}
		for _, member := range strings.Split(fields[3], ",") {
			if member == name {
				groups = append(groups, uint32(gid))
				break
			}
		}
	}
	return groups
}

// applyUser resolves the template user after the overlay pivot. A restore that
// cannot run as the user fails, so nothing runs as root by accident.
func applyUser(spec string) error {
	var u *runAs
	if spec != "" {
		var err error
		if u, err = lookupUser(spec, passwdPath, groupPath); err != nil {
			return err
		}
	}
	resumeMu.Lock()
	templateUser = u
	resumeMu.Unlock()
	return nil
}

// sysProcAttr adds the template user to the attributes of one child.
func withUser(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if u := currentUser(); u != nil {
		attr.Credential = &syscall.Credential{Uid: u.uid, Gid: u.gid, Groups: u.groups}
	}
	return attr
}

// processEnv is the environment of one child. HOME and USER come first, so
// the secrets and the request can still replace them.
func processEnv(extra map[string]string) []string {
	merged := map[string]string{}
	if u := currentUser(); u != nil {
		merged["HOME"] = u.home
		merged["USER"] = u.name
		merged["LOGNAME"] = u.name
	}
	for k, v := range trustEnv() {
		merged[k] = v
	}
	for k, v := range injectedSecrets() {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return guestproto.ExecEnv(nil, merged)
}

// dropFilesystemIdentity makes this thread reach files as the template user.
// The kernel clears the filesystem capabilities with a move away from fsuid
// 0, so a symlink an agent planted cannot aim a root write at a root-owned
// file. These calls change one thread only, and the thread stays locked: the
// runtime ends it when this goroutine returns, so no other request inherits
// the identity.
func dropFilesystemIdentity() error {
	u := currentUser()
	if u == nil {
		return nil
	}
	runtime.LockOSThread()
	if len(u.groups) > 0 {
		_, _, errno := unix.RawSyscall(unix.SYS_SETGROUPS, uintptr(len(u.groups)), uintptr(unsafe.Pointer(&u.groups[0])), 0)
		if errno != 0 {
			return fmt.Errorf("setgroups: %w", errno)
		}
	}
	unix.Setfsgid(int(u.gid))
	unix.Setfsuid(int(u.uid))
	return nil
}

// runCheckUser answers whether the image holds a user, while a template builds.
func runCheckUser(conn net.Conn, req *guestproto.Request) {
	if _, err := lookupUser(req.User, passwdPath, groupPath); err != nil {
		writeResult(conn, guestproto.Result{Error: &guestproto.Error{Code: guestproto.CodeInvalid, Message: err.Error()}})
		return
	}
	writeResult(conn, guestproto.Result{OK: true})
}
