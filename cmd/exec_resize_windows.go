//go:build windows

package cmd

import "github.com/redentordev/tako-cli/pkg/takoapi/ptystream"

// watchExecResize is a no-op on Windows: there is no SIGWINCH. The remote
// PTY keeps its initial size. The channel is closed on stop.
func watchExecResize(_ int, resize chan<- ptystream.Winsize) func() {
	done := make(chan struct{})
	go func() {
		<-done
		close(resize)
	}()
	return func() { close(done) }
}
