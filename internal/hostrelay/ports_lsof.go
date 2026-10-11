//go:build !linux

package hostrelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// lsofTimeout bounds one lsof run.
const lsofTimeout = 10 * time.Second

// lsofCommand is the lsof run (a var for tests): every listening TCP socket
// of this user's processes, numeric, as fields (pid, command, type, name).
var lsofCommand = func(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pctn")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(out) == 0 {
		// lsof exits 1 when it lists nothing.
		return nil, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("listing the listening ports needs lsof on the daemon host")
	}
	return out, err
}

// listeners lists the listening TCP sockets lsof shows (this user's
// processes'), each marked when its process is fleet's.
func (s *Scanner) listeners(ctx context.Context) ([]Listener, error) {
	ctx, cancel := context.WithTimeout(ctx, lsofTimeout)
	defer cancel()
	out, err := lsofCommand(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the listening ports: %w", err)
	}
	return parseLsof(bytes.NewReader(out), func(pid int, command string) bool {
		return pid == os.Getpid() || s.isSelf(command)
	})
}
