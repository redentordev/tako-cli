//go:build windows

package projectbinding

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

var secureBindingWindowsTestHook func(string)

func readSecureBindingFileOptional(path string) ([]byte, error) {
	dirHandle, dirInfo, err := openSecureBindingDirectoryWindows(filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(dirHandle)
	if secureBindingWindowsTestHook != nil {
		secureBindingWindowsTestHook("read-directory-open")
	}

	openedPath, err := windowsDirectoryChildPath(dirHandle, filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("resolve project cluster binding in opened directory: %w", err)
	}
	file, opened, err := openBindingFileWindows(openedPath)
	if errors.Is(err, os.ErrNotExist) {
		if verifyErr := verifyBindingDirectoryPathWindows(filepath.Dir(path), dirInfo); verifyErr != nil {
			return nil, verifyErr
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentSize+1))
	if err != nil {
		return nil, fmt.Errorf("read project cluster binding: %w", err)
	}
	if len(data) > maxDocumentSize {
		return nil, fmt.Errorf("project cluster binding exceeds %d bytes", maxDocumentSize)
	}
	currentPath, err := windowsDirectoryChildPath(dirHandle, filepath.Base(path))
	if err != nil {
		return nil, fmt.Errorf("resolve project cluster binding for revalidation: %w", err)
	}
	currentFile, current, err := openBindingFileWindows(currentPath)
	if err != nil {
		return nil, fmt.Errorf("revalidate project cluster binding: %w", err)
	}
	currentFile.Close()
	if !sameWindowsFile(opened, current) {
		return nil, fmt.Errorf("project cluster binding changed while it was read")
	}
	if err := verifyBindingDirectoryPathWindows(filepath.Dir(path), dirInfo); err != nil {
		return nil, err
	}
	return data, nil
}

func createSecureBindingFile(path string, data []byte) ([]byte, error) {
	dir := filepath.Dir(path)
	dirHandle, dirInfo, err := openSecureBindingDirectoryWindows(dir, true)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(dirHandle)

	openedDirPath, err := windowsFinalPath(dirHandle)
	if err != nil {
		return nil, fmt.Errorf("resolve opened Tako workspace state directory: %w", err)
	}
	tmp, err := os.CreateTemp(openedDirPath, ".platform-cluster.*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create project cluster binding temporary file: %w", err)
	}
	tmpName := filepath.Base(tmp.Name())
	tmpCleanupPath := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			if currentPath, pathErr := windowsFinalPath(windows.Handle(tmp.Fd())); pathErr == nil {
				tmpCleanupPath = currentPath
			}
			_ = tmp.Close()
		}
		_ = os.Remove(tmpCleanupPath)
		if currentPath, pathErr := windowsDirectoryChildPath(dirHandle, tmpName); pathErr == nil {
			_ = os.Remove(currentPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		return nil, fmt.Errorf("protect project cluster binding temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return nil, fmt.Errorf("write project cluster binding temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("sync project cluster binding temporary file: %w", err)
	}
	var staged windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(tmp.Fd()), &staged); err != nil {
		return nil, fmt.Errorf("inspect staged project cluster binding: %w", err)
	}
	if err := validateWindowsBindingInfo(staged); err != nil {
		return nil, err
	}
	tmpCleanupPath, err = windowsFinalPath(windows.Handle(tmp.Fd()))
	if err != nil {
		return nil, fmt.Errorf("resolve staged project cluster binding: %w", err)
	}
	tmpParentHandle, tmpParentInfo, err := openSecureBindingDirectoryWindows(filepath.Dir(tmpCleanupPath), false)
	if err != nil {
		return nil, fmt.Errorf("validate staged project cluster binding directory: %w", err)
	}
	windows.CloseHandle(tmpParentHandle)
	if !sameWindowsFile(dirInfo, tmpParentInfo) {
		return nil, fmt.Errorf("Tako workspace state directory changed before temporary binding creation")
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close project cluster binding temporary file: %w", err)
	}
	tmpOpen = false
	if secureBindingWindowsTestHook != nil {
		secureBindingWindowsTestHook("create-temporary-synced")
	}
	if err := verifyBindingDirectoryPathWindows(dir, dirInfo); err != nil {
		return nil, err
	}

	openedDirPath, err = windowsFinalPath(dirHandle)
	if err != nil {
		return nil, fmt.Errorf("resolve opened Tako workspace state directory: %w", err)
	}
	tmpPath := filepath.Join(openedDirPath, tmpName)
	base := filepath.Base(path)
	targetPath := filepath.Join(openedDirPath, base)
	linkErr := os.Link(tmpPath, targetPath)
	created := linkErr == nil
	if linkErr != nil && !errors.Is(linkErr, os.ErrExist) {
		if verifyErr := verifyBindingDirectoryPathWindows(dir, dirInfo); verifyErr != nil {
			return nil, verifyErr
		}
		return nil, fmt.Errorf("publish create-once project cluster binding: %w", linkErr)
	}
	if unlinkErr := os.Remove(tmpPath); unlinkErr != nil {
		if created {
			if rollbackErr := removeWindowsBindingIfSame(dirHandle, base, staged); rollbackErr != nil {
				return nil, fmt.Errorf("project cluster binding publication outcome is ambiguous after temporary-link cleanup failed (%v) and rollback failed: %w", unlinkErr, rollbackErr)
			}
		}
		return nil, fmt.Errorf("remove project cluster binding temporary link after publication rollback: %w", unlinkErr)
	}
	winner, err := readWindowsBindingAtDirectory(dirHandle, base)
	if err != nil {
		if created {
			if rollbackErr := removeWindowsBindingIfSame(dirHandle, base, staged); rollbackErr != nil {
				return nil, fmt.Errorf("read published project cluster binding failed (%v); rollback failed: %w", err, rollbackErr)
			}
		}
		return nil, err
	}
	if secureBindingWindowsTestHook != nil {
		secureBindingWindowsTestHook("create-winner-read")
	}
	if err := verifyBindingDirectoryPathWindows(dir, dirInfo); err != nil {
		if created {
			if rollbackErr := removeWindowsBindingIfSame(dirHandle, base, staged); rollbackErr != nil {
				return nil, fmt.Errorf("%v; project cluster binding rollback failed: %w", err, rollbackErr)
			}
		}
		return nil, err
	}
	return winner, nil
}

func readWindowsBindingAtDirectory(dirHandle windows.Handle, name string) ([]byte, error) {
	path, err := windowsDirectoryChildPath(dirHandle, name)
	if err != nil {
		return nil, err
	}
	file, opened, err := openBindingFileWindows(path)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentSize+1))
	file.Close()
	if err != nil {
		return nil, fmt.Errorf("read project cluster binding: %w", err)
	}
	if len(data) > maxDocumentSize {
		return nil, fmt.Errorf("project cluster binding exceeds %d bytes", maxDocumentSize)
	}
	currentPath, err := windowsDirectoryChildPath(dirHandle, name)
	if err != nil {
		return nil, err
	}
	currentFile, current, err := openBindingFileWindows(currentPath)
	if err != nil {
		return nil, fmt.Errorf("revalidate project cluster binding: %w", err)
	}
	currentFile.Close()
	if !sameWindowsFile(opened, current) {
		return nil, fmt.Errorf("project cluster binding changed while it was read")
	}
	return data, nil
}

func openSecureBindingDirectoryWindows(path string, create bool) (windows.Handle, windows.ByHandleFileInformation, error) {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return windows.InvalidHandle, windows.ByHandleFileInformation{}, fmt.Errorf("create Tako workspace state directory: %w", err)
		}
	}
	handle, info, err := openWindowsPath(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		if isWindowsNotExist(err) {
			return windows.InvalidHandle, windows.ByHandleFileInformation{}, os.ErrNotExist
		}
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, fmt.Errorf("open Tako workspace state directory without following reparse points: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, fmt.Errorf("Tako workspace state path must be a directory and not a reparse point")
	}
	return handle, info, nil
}

func verifyBindingDirectoryPathWindows(path string, expected windows.ByHandleFileInformation) error {
	handle, current, err := openSecureBindingDirectoryWindows(path, false)
	if err != nil {
		return fmt.Errorf("revalidate Tako workspace state directory: %w", err)
	}
	windows.CloseHandle(handle)
	if !sameWindowsFile(expected, current) {
		return fmt.Errorf("Tako workspace state directory changed during binding operation")
	}
	return nil
}

func openBindingFileWindows(path string) (*os.File, windows.ByHandleFileInformation, error) {
	handle, info, err := openWindowsPath(path, windows.FILE_GENERIC_READ, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		if isWindowsNotExist(err) {
			return nil, windows.ByHandleFileInformation{}, os.ErrNotExist
		}
		return nil, windows.ByHandleFileInformation{}, fmt.Errorf("open project cluster binding without following reparse points: %w", err)
	}
	if err := validateWindowsBindingInfo(info); err != nil {
		windows.CloseHandle(handle)
		return nil, windows.ByHandleFileInformation{}, err
	}
	return os.NewFile(uintptr(handle), path), info, nil
}

func validateWindowsBindingInfo(info windows.ByHandleFileInformation) error {
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("project cluster binding must be a regular file and not a reparse point")
	}
	if info.NumberOfLinks != 1 {
		return fmt.Errorf("project cluster binding must have exactly one link, got %d", info.NumberOfLinks)
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size > maxDocumentSize {
		return fmt.Errorf("project cluster binding exceeds %d bytes", maxDocumentSize)
	}
	return nil
}

func openWindowsPath(path string, access uint32, flags uint32) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	handle, err := windows.CreateFile(name, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, flags, 0)
	if err != nil {
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return windows.InvalidHandle, windows.ByHandleFileInformation{}, err
	}
	return handle, info, nil
}

func windowsFinalPath(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buffer)) {
			return windows.UTF16ToString(buffer[:n]), nil
		}
		buffer = make([]uint16, n+1)
	}
}

func windowsDirectoryChildPath(handle windows.Handle, name string) (string, error) {
	dir, err := windowsFinalPath(handle)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func removeWindowsBindingIfSame(dirHandle windows.Handle, name string, expected windows.ByHandleFileInformation) error {
	path, err := windowsDirectoryChildPath(dirHandle, name)
	if err != nil {
		return err
	}
	file, current, err := openBindingFileWindows(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	file.Close()
	if !sameWindowsFile(expected, current) {
		return fmt.Errorf("published project cluster binding changed before rollback")
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove published project cluster binding during rollback: %w", err)
	}
	return nil
}

func sameWindowsFile(left, right windows.ByHandleFileInformation) bool {
	return left.VolumeSerialNumber == right.VolumeSerialNumber && left.FileIndexHigh == right.FileIndexHigh && left.FileIndexLow == right.FileIndexLow
}

func isWindowsNotExist(err error) bool {
	return errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}
