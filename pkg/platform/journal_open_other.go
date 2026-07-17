//go:build !linux && !darwin

package platform

import "os"

func openJournalAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0640)
}
