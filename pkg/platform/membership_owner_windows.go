//go:build windows

package platform

import "os"

func validateMembershipDirectoryOwner(os.FileInfo) error { return nil }
func validateMembershipFileOwner(os.FileInfo) error      { return nil }
