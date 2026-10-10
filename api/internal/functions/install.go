package functions

import (
	"errors"
	"os"
	"path"
)

// InstallBundle validates all entries before creating any executable fragment.
// The caller owns an exclusive guest deployment directory. A pre-existing
// digest is refused rather than adopting files written by another generation.
// os.Root keeps every operation below the trusted parent, even if a path changes.
func InstallBundle(archive []byte, parent string) (Bundle, error) {
	bundle, err := ValidateBundle(archive)
	if err != nil {
		return Bundle{}, err
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return Bundle{}, errors.New("trusted bundle parent unavailable")
	}
	defer root.Close()
	directory := "bundle-" + bundle.Digest
	if err = root.Mkdir(directory, 0755); err != nil {
		return Bundle{}, errors.New("fresh immutable bundle destination required")
	}
	complete := false
	defer func() {
		if !complete {
			_ = root.RemoveAll(directory)
		}
	}()
	for _, file := range bundle.Files {
		name := path.Join(directory, file.Name)
		if err = root.MkdirAll(path.Dir(name), 0755); err != nil {
			return Bundle{}, errors.New("bundle directory creation failed")
		}
		output, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0444)
		if err != nil {
			return Bundle{}, errors.New("immutable bundle file creation failed")
		}
		_, writeErr := output.Write(file.Content)
		syncErr := output.Sync()
		closeErr := output.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return Bundle{}, errors.New("bundle persistence failed")
		}
	}
	// Seal directories as well as files. Node UID 65532 must not own these
	// paths; the privileged guest caller verifies its ownership before launch.
	for _, file := range bundle.Files {
		for folder := path.Dir(path.Join(directory, file.Name)); folder != "."; folder = path.Dir(folder) {
			if err = root.Chmod(folder, 0555); err != nil {
				return Bundle{}, errors.New("bundle sealing failed")
			}
		}
	}
	complete = true
	return bundle, nil
}
