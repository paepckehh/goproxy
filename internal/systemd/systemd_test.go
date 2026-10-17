package systemd

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// The helper-process tests re-execute the test binary the same way systemd
// re-executes the daemon: listening descriptors arrive as fds 3+ (via
// ExtraFiles) with LISTEN_FDS set, and the child fills in LISTEN_PID for
// its own pid, then reports the result over stdout.

func TestListenersWithoutActivation(t *testing.T) {
	for _, tt := range []struct {
		name      string
		listenFDs string
		listenPID string
		wantNil   bool
	}{
		{"no environment", "", "", true},
		{"fd count without pid", "1", "", true},
		{"pid without fd count", "", strconv.Itoa(os.Getpid()), true},
		{"zero fds", "0", strconv.Itoa(os.Getpid()), true},
		{"negative fds", "-1", strconv.Itoa(os.Getpid()), true},
		{"garbage fd count", "x", strconv.Itoa(os.Getpid()), true},
		{"foreign pid", "1", "1", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.listenFDs == "" {
				os.Unsetenv("LISTEN_FDS")
			} else {
				os.Setenv("LISTEN_FDS", tt.listenFDs)
			}
			if tt.listenPID == "" {
				os.Unsetenv("LISTEN_PID")
			} else {
				os.Setenv("LISTEN_PID", tt.listenPID)
			}
			t.Cleanup(func() {
				os.Unsetenv("LISTEN_FDS")
				os.Unsetenv("LISTEN_PID")
			})
			ls, err := Listeners()
			if err != nil {
				t.Fatalf("Listeners() error: %v", err)
			}
			if tt.wantNil && ls != nil {
				t.Fatalf("Listeners() = %v, want nil", ls)
			}
		})
	}
}

func TestListenersFromParent(t *testing.T) {
	if os.Getenv("GOPROXY_SYSTEMD_TEST_CHILD") == "1" {
		listenersChild()
		return
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tf, err := ln.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer tf.Close()

	uln, err := net.Listen("unix", t.TempDir()+"/sock")
	if err != nil {
		t.Fatal(err)
	}
	defer uln.Close()
	uf, err := uln.(*net.UnixListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer uf.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestListenersFromParent")
	cmd.Env = append(os.Environ(), "GOPROXY_SYSTEMD_TEST_CHILD=1", "LISTEN_FDS=2")
	cmd.ExtraFiles = []*os.File{tf, uf}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	// The child owns duplicate descriptors of the same sockets, so a
	// connection dialed here must be served by the child's Accept.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		cmd.Process.Kill()
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if string(buf) != "x" {
		t.Fatalf("echo = %q, want %q", buf, "x")
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("child failed: %v\n%s", err, out.String())
	}
	lines := strings.Fields(out.String())
	want := []string{
		"CHILD:N=2",
		"CHILD:ADDR=" + ln.Addr().String(),
		"CHILD:ADDR=" + uln.Addr().String(),
		"CHILD:FDS_ENV=",
	}
	for _, w := range want {
		found := false
		for _, l := range lines {
			if l == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("child output missing %q in:\n%s", w, out.String())
		}
	}
}

func listenersChild() {
	os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	ls, err := Listeners()
	if err != nil {
		fmt.Printf("CHILD:ERR=%v\n", err)
		return
	}
	fmt.Printf("CHILD:N=%d\n", len(ls))
	for _, l := range ls {
		fmt.Printf("CHILD:ADDR=%s\n", l.Addr().String())
	}
	fmt.Printf("CHILD:FDS_ENV=%s\n", os.Getenv("LISTEN_FDS"))

	c, err := ls[0].Accept()
	if err != nil {
		fmt.Printf("CHILD:ACCEPT_ERR=%v\n", err)
		return
	}
	defer c.Close()
	buf := make([]byte, 1)
	if _, err := c.Read(buf); err != nil {
		fmt.Printf("CHILD:READ_ERR=%v\n", err)
		return
	}
	c.Write(buf)
}

func TestListenersNonListenerFD(t *testing.T) {
	if os.Getenv("GOPROXY_SYSTEMD_TEST_PIPE_CHILD") == "1" {
		os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
		_, err := Listeners()
		fmt.Printf("CHILD:ERR=%v\n", err)
		return
	}

	_, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=TestListenersNonListenerFD")
	cmd.Env = append(os.Environ(), "GOPROXY_SYSTEMD_TEST_PIPE_CHILD=1", "LISTEN_FDS=1")
	cmd.ExtraFiles = []*os.File{w}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "CHILD:ERR=systemd socket activation") {
		t.Fatalf("child output = %q, want fd error", out)
	}
}
