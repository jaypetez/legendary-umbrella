//go:build !windows

package agent

import (
	"io"
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixPTY struct {
	f   *os.File
	cmd *exec.Cmd
}

func openPTY(shell string, args []string, rows, cols uint16) (ptyHost, error) {
	cmd := exec.Command(shell, args...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, err
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }
func (u *unixPTY) Resize(rows, cols uint16) error {
	return pty.Setsize(u.f, &pty.Winsize{Rows: rows, Cols: cols})
}
func (u *unixPTY) Close() error {
	_ = u.f.Close()
	if u.cmd.Process != nil {
		_ = u.cmd.Process.Kill()
		_, _ = u.cmd.Process.Wait()
	}
	return nil
}

// Compile-time interface check.
var _ interface {
	io.Reader
	io.Writer
	io.Closer
} = (*unixPTY)(nil)
