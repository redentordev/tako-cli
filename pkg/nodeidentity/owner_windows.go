//go:build windows

package nodeidentity

import "os"

func validateOwner(_ os.FileInfo) error {
	return nil
}
