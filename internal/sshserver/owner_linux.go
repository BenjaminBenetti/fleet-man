package sshserver

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// socketOwnedBy reports whether the endpoint a forward reached, a TCP port
// or a unix socket's path in the instance, is listened on by a process
// whose executable is under one of dirs: VS Code's server, when dirs are
// its install directories. It reads the instance's /proc: the listening
// sockets of its network namespace, then the descriptors of the processes
// it may look at (the account's own, VS Code's among them).
func socketOwnedBy(network, address string, dirs []string) bool {
	var inodes map[string]bool
	switch network {
	case "tcp":
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(port)
		if err != nil {
			return false
		}
		inodes = listeningTCP("/proc/net/tcp", n)
		for k := range listeningTCP("/proc/net/tcp6", n) {
			inodes[k] = true
		}
	case "unix":
		inodes = listeningUnix("/proc/net/unix", address)
	}
	if len(inodes) == 0 {
		return false
	}
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pid := range pids {
		fds, err := os.ReadDir(filepath.Join(pid, "fd"))
		if err != nil {
			continue
		}
		owns := false
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(pid, "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			if inodes[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] {
				owns = true
				break
			}
		}
		if !owns {
			continue
		}
		exe, err := os.Readlink(filepath.Join(pid, "exe"))
		if err != nil {
			continue
		}
		exe = strings.TrimSuffix(exe, " (deleted)")
		for _, dir := range dirs {
			if strings.HasPrefix(exe, strings.TrimSuffix(dir, "/")+"/") {
				return true
			}
		}
	}
	return false
}

// runsFrom reports whether a process the account may look at runs an
// executable under dir (a VS Code server install directory: its node, once
// the server started).
func runsFrom(dir string) bool {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	prefix := strings.TrimSuffix(real, "/") + "/"
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pid := range pids {
		exe, err := os.Readlink(filepath.Join(pid, "exe"))
		if err == nil && strings.HasPrefix(strings.TrimSuffix(exe, " (deleted)"), prefix) {
			return true
		}
	}
	return false
}

// listeningTCP are the inodes of the sockets listening on port in a
// /proc/net/tcp-format table.
func listeningTCP(table string, port int) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(table)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // TCP_LISTEN
			continue
		}
		_, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		if p, err := strconv.ParseUint(hexPort, 16, 16); err == nil && int(p) == port {
			out[fields[9]] = true
		}
	}
	return out
}

// listeningUnix are the inodes of the listening unix sockets bound at path
// in /proc/net/unix.
func listeningUnix(table, path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(table)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // the header
	for sc.Scan() {
		// Num RefCount Protocol Flags Type St Inode Path
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[7] != path {
			continue
		}
		if flags, err := strconv.ParseUint(fields[3], 16, 32); err == nil && flags&0x10000 != 0 { // __SO_ACCEPTCON
			out[fields[6]] = true
		}
	}
	return out
}
