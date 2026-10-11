package sshserver

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// Account is whom the server's sessions run as: the user the server itself
// runs as (the devcontainer's remoteUser), with the home and login shell
// sessions start in.
type Account struct {
	Name  string
	Home  string
	Shell string
}

// passwdPath is the account database the login shell is read from (a var
// for tests).
var passwdPath = "/etc/passwd"

// CurrentAccount is the account of the process: its name and home from the
// user database, its login shell from /etc/passwd, else $SHELL, else
// /bin/sh.
func CurrentAccount() (*Account, error) {
	uid := os.Getuid()
	acct := &Account{Home: os.Getenv("HOME"), Name: os.Getenv("USER")}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		acct.Name, acct.Home = u.Username, u.HomeDir
	}
	if name, home, shell, ok := passwdEntry(uid); ok {
		if acct.Name == "" {
			acct.Name = name
		}
		if acct.Home == "" {
			acct.Home = home
		}
		acct.Shell = shell
	}
	if acct.Shell == "" {
		acct.Shell = os.Getenv("SHELL")
	}
	if acct.Shell == "" {
		acct.Shell = "/bin/sh"
	}
	if acct.Home == "" {
		return nil, fmt.Errorf("uid %d has no home directory", uid)
	}
	return acct, nil
}

// passwdEntry is uid's name, home and shell in /etc/passwd.
func passwdEntry(uid int) (name, home, shell string, ok bool) {
	f, err := os.Open(passwdPath)
	if err != nil {
		return "", "", "", false
	}
	defer f.Close()
	want := strconv.Itoa(uid)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ":")
		if len(fields) < 7 || fields[2] != want {
			continue
		}
		return fields[0], fields[5], fields[6], true
	}
	return "", "", "", false
}
