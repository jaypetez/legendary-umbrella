//go:build windows

package agent

import (
	"strings"

	"github.com/UserExistsError/conpty"
)

type windowsPTY struct {
	c *conpty.ConPty
}

func openPTY(shell string, args []string, rows, cols uint16) (ptyHost, error) {
	// conpty takes a full command line, not a program + args slice.
	parts := append([]string{shell}, args...)
	cmdline := quoteCmdline(parts)
	c, err := conpty.Start(cmdline, conpty.ConPtyDimensions(int(cols), int(rows)))
	if err != nil {
		return nil, err
	}
	return &windowsPTY{c: c}, nil
}

func (w *windowsPTY) Read(p []byte) (int, error)  { return w.c.Read(p) }
func (w *windowsPTY) Write(p []byte) (int, error) { return w.c.Write(p) }
func (w *windowsPTY) Resize(rows, cols uint16) error {
	return w.c.Resize(int(cols), int(rows))
}
func (w *windowsPTY) Close() error { return w.c.Close() }

// quoteCmdline joins a program + args list into a Windows command line string.
// It's a minimal implementation — sufficient for shell paths we control. For
// anything with embedded quotes or backslashes we'd need the full CommandLineToArgvW
// rules, but cmd.exe / powershell.exe paths don't trigger those.
func quoteCmdline(parts []string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.ContainsAny(p, " \t") {
			out = append(out, `"`+p+`"`)
		} else {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}
