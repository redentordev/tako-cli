package takod

import (
	"os"

	"github.com/redentordev/tako-cli/pkg/fileutil"
)

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	return fileutil.WriteFileAtomic(path, data, mode)
}

func syncDirectory(path string) error {
	return fileutil.SyncDirectory(path)
}
