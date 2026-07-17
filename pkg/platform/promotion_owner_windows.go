//go:build windows

package platform

import "os"

func validatePassivePromotionAncestorOwner(os.FileInfo) error { return nil }
