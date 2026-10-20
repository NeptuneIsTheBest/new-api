package common

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	diskCacheRootLock  = ".directory.lock"
	diskCacheOwnerLock = ".owner-v1.lock"
)

// All participants acquire locks in this order: manager mutex, root lock, owner
// lock. The root lock file is never removed, so waiters always lock the same inode.
func acquireDiskCacheLock(path string, create, nonblocking bool) (*os.File, error) {
	file, err := openDiskCacheLockFile(path, create)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("cache lock is not a regular file: %s", path)
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	locked, err := lockDiskCacheFile(file, nonblocking)
	if err != nil || !locked {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func closeDiskCacheLock(file *os.File) error {
	return errors.Join(unlockDiskCacheFile(file), file.Close())
}

func (m *diskCacheManager) scanErrorLocked() error {
	var result error
	for _, err := range m.roots {
		result = errors.Join(result, err)
	}
	return result
}

func (m *diskCacheManager) ensureDirectoryLocked(dir string) (result error) {
	root := filepath.Dir(dir)
	if _, known := m.roots[root]; !known {
		m.roots[root] = nil
	}
	directory := m.directories[dir]
	if directory != nil && directory.owner != nil && m.scanErrorLocked() == nil {
		return nil
	}
	if err := m.syncDirectoriesLocked(nil); err != nil {
		return err
	}
	directory = m.directories[dir]
	if directory != nil && directory.owner != nil {
		return nil
	}
	defer func() { m.roots[root] = result }()
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("cache root is not a directory: %s", root)
	}
	rootLock, err := acquireDiskCacheLock(filepath.Join(root, diskCacheRootLock), true, false)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, closeDiskCacheLock(rootLock)) }()
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	return m.syncProcessDirectoryLocked(dir, nil, true)
}

// A nil age reconciles accounting without deleting cached payloads. Remember
// roots even after their directories empty, so later exits there are discovered.
func (m *diskCacheManager) syncDirectoriesLocked(maxAge *time.Duration) error {
	m.initialized = true
	current := m.config.directory()
	root := filepath.Dir(current)
	if _, known := m.roots[root]; !known {
		m.roots[root] = nil
	}
	if m.directories[current] == nil {
		m.directories[current] = &diskCacheDirectory{}
	}
	for root := range m.roots {
		m.roots[root] = m.syncRootLocked(root, maxAge)
	}
	return m.scanErrorLocked()
}

func (m *diskCacheManager) syncRootLocked(root string, maxAge *time.Duration) (result error) {
	var entries []os.DirEntry
	info, err := os.Lstat(root)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("cache root is not a directory: %s", root)
		}
		rootLock, err := acquireDiskCacheLock(filepath.Join(root, diskCacheRootLock), true, false)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, closeDiskCacheLock(rootLock)) }()
		entries, err = os.ReadDir(root)
		if err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		id, ok := strings.CutPrefix(entry.Name(), "process-")
		if !ok || len(id) != 36 || uuid.Validate(id) != nil {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		seen[dir] = true
		if !entry.IsDir() {
			if m.directories[dir] != nil {
				result = errors.Join(result, fmt.Errorf("cache path is not a directory: %s", dir))
			}
			continue
		}
		result = errors.Join(result, m.syncProcessDirectoryLocked(dir, maxAge, dir == m.config.directory()))
	}
	for dir, directory := range m.directories {
		if filepath.Dir(dir) != root || seen[dir] {
			continue
		}
		// Another cleaner may have removed a previously counted orphan. Active
		// files keep their charge until their last reader/writer closes.
		result = errors.Join(result, m.scanDirectoryLocked(dir))
		if dir != m.config.directory() && directory.count == 0 && directory.err == nil {
			if directory.owner != nil {
				result = errors.Join(result, closeDiskCacheLock(directory.owner))
			}
			delete(m.directories, dir)
		}
	}
	return result
}

// Called with the root lock held. Only our UUID may create an owner marker;
// unmarked directories from older versions have no safe liveness protocol.
func (m *diskCacheManager) syncProcessDirectoryLocked(dir string, maxAge *time.Duration, keepOwner bool) (result error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("cache path is not a directory: %s", dir)
	}
	directory := m.directories[dir]
	ours := filepath.Base(dir) == diskCacheProcessDir
	var lease *os.File
	if directory == nil || directory.owner == nil {
		lease, err = acquireDiskCacheLock(filepath.Join(dir, diskCacheOwnerLock), ours, true)
		if !ours && os.IsNotExist(err) && directory == nil {
			return nil
		}
		if err != nil {
			return err
		}
		if lease == nil {
			if ours {
				return fmt.Errorf("cache directory is already owned: %s", dir)
			}
			return nil
		}
		if directory == nil {
			directory = &diskCacheDirectory{}
			m.directories[dir] = directory
		}
		if ours {
			directory.owner = lease
			lease = nil
		}
	}
	defer func() {
		if lease != nil {
			result = errors.Join(result, closeDiskCacheLock(lease))
		}
	}()
	if err := m.scanDirectoryLocked(dir); err != nil {
		return err
	}
	if maxAge != nil {
		now := time.Now()
		for _, entry := range m.files {
			if filepath.Dir(entry.path) == dir && entry.references == 0 && now.Sub(entry.inactiveAt) > *maxAge {
				result = errors.Join(result, m.removeLocked(entry))
			}
		}
	}
	if result != nil || directory.count != 0 || keepOwner || (!ours && maxAge == nil) {
		return result
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if ours {
		lease = directory.owner
		directory.owner = nil
	}
	// Empty retired directories no longer need a lifetime lock. Keep the marker
	// if unknown contents remain, and never recursively remove those contents.
	if len(entries) != 1 || entries[0].Name() != diskCacheOwnerLock {
		delete(m.directories, dir)
		return nil
	}
	err = closeDiskCacheLock(lease)
	lease = nil
	if err != nil {
		return err
	}
	marker := filepath.Join(dir, diskCacheOwnerLock)
	if err := os.Remove(marker); err != nil {
		return err
	}
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		// Preserve the protocol marker so a failed directory removal is retryable.
		lease, result = acquireDiskCacheLock(marker, true, true)
		return errors.Join(err, result)
	}
	delete(m.directories, dir)
	return nil
}
