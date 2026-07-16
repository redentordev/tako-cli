//go:build !linux && !darwin

package platform

import "os"

func lockJournalFile(*os.File) (func(), error) { return func() {}, nil }
