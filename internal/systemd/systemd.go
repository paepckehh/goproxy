// Package systemd implements the one piece of the systemd service-manager
// interface goproxy needs: socket activation via the LISTEN_FDS protocol
// (sd_listen_fds), without any external dependency.
package systemd

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

// firstListenFD is the descriptor systemd uses for the first passed socket
// (SD_LISTEN_FDS_START).
const firstListenFD = 3

// Listeners returns the listening sockets systemd passed to this process
// via socket activation. systemd sets LISTEN_PID to the daemon's pid and
// LISTEN_FDS to the socket count, with the descriptors starting at 3.
// It returns nil when the process was not socket-activated; a descriptor
// that is not a listener returns an error rather than being ignored.
//
// The returned listeners own duplicates of the descriptors; the originals
// are closed, and the protocol variables are unset, so nothing leaks into
// child processes or a second activation attempt.
func Listeners() ([]net.Listener, error) {
	pid, err := strconv.Atoi(os.Getenv("LISTEN_PID"))
	if err != nil || pid != os.Getpid() {
		return nil, nil
	}
	fds, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || fds <= 0 {
		return nil, nil
	}
	os.Unsetenv("LISTEN_PID")
	os.Unsetenv("LISTEN_FDS")

	listeners := make([]net.Listener, 0, fds)
	for fd := firstListenFD; fd < firstListenFD+fds; fd++ {
		f := os.NewFile(uintptr(fd), fmt.Sprintf("listen-fd-%d", fd))
		if f == nil {
			return nil, fmt.Errorf("systemd socket activation: fd %d unavailable", fd)
		}
		l, err := net.FileListener(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("systemd socket activation: fd %d: %w", fd, err)
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}
