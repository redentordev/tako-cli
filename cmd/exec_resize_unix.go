//go:build !windows

package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/redentordev/tako-cli/pkg/takoapi/ptystream"
	"golang.org/x/term"
)

// watchExecResize forwards SIGWINCH terminal sizes into resize until the
// returned stop function runs. The channel is closed on stop.
func watchExecResize(fd int, resize chan<- ptystream.Winsize) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)

	done := make(chan struct{})
	go func() {
		defer close(resize)
		for {
			select {
			case <-sigCh:
				if w, h, err := term.GetSize(fd); err == nil {
					select {
					case resize <- ptystream.Winsize{Cols: uint16(w), Rows: uint16(h)}:
					case <-done:
						return
					}
				}
			case <-done:
				return
			}
		}
	}()

	return func() {
		signal.Stop(sigCh)
		close(done)
	}
}
