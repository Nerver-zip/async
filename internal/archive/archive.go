package archive

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PackDirTarGz writes the contents of srcDir as a gzip-compressed tar archive to w.
func PackDirTarGz(srcDir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	defer gz.Close()

	tw := tar.NewWriter(gz)
	defer tw.Close()

	root := filepath.Clean(srcDir)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = relSlash
		if d.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(tw, f)
		return err
	})
}

// UnpackTarGzToStaged unpacks a tar.gz stream into a newly created temporary staging directory
// inside parentDir, returning the staging directory path and a cleanup closure.
func UnpackTarGzToStaged(r io.Reader, parentDir string) (string, func(), error) {
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("creating parent dir %s: %w", parentDir, err)
	}

	stagingDir, err := os.MkdirTemp(parentDir, ".async-staging-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating staging dir in %s: %w", parentDir, err)
	}

	cleanup := func() {
		_ = os.RemoveAll(stagingDir)
	}

	if err := UnpackTarGz(r, stagingDir); err != nil {
		cleanup()
		return "", nil, err
	}
	return stagingDir, cleanup, nil
}

// UnpackTarGz extracts a gzip-compressed tar stream into destDir, rejecting any path traversal outside destDir.
func UnpackTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("opening gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanDest := filepath.Clean(destDir)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar header: %w", err)
		}

		rel := filepath.FromSlash(hdr.Name)
		target := filepath.Join(cleanDest, rel)
		if !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) && target != cleanDest {
			return fmt.Errorf("illegal path in archive: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("creating dir %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent dir for %s: %w", target, err)
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return fmt.Errorf("writing file %s: %w", target, err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("closing file %s: %w", target, err)
			}
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			}
		}
	}
	return nil
}

// AtomicInstallFile copies or renames srcFile into dstFile atomically via a temporary sibling file.
func AtomicInstallFile(srcFile, dstFile string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {
		return fmt.Errorf("creating parent dir for %s: %w", dstFile, err)
	}

	tmpDst, err := os.CreateTemp(filepath.Dir(dstFile), ".async-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", filepath.Dir(dstFile), err)
	}
	tmpPath := tmpDst.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()

	src, err := os.Open(srcFile)
	if err != nil {
		_ = tmpDst.Close()
		return fmt.Errorf("opening source file %s: %w", srcFile, err)
	}
	defer src.Close()

	if _, err := io.Copy(tmpDst, src); err != nil {
		_ = tmpDst.Close()
		return fmt.Errorf("copying to temp file %s: %w", tmpPath, err)
	}
	if err := tmpDst.Chmod(mode); err != nil {
		_ = tmpDst.Close()
		return fmt.Errorf("chmod temp file %s: %w", tmpPath, err)
	}
	if err := tmpDst.Sync(); err != nil {
		_ = tmpDst.Close()
		return fmt.Errorf("syncing temp file %s: %w", tmpPath, err)
	}
	if err := tmpDst.Close(); err != nil {
		return fmt.Errorf("closing temp file %s: %w", tmpPath, err)
	}

	if st, err := os.Stat(srcFile); err == nil {
		_ = os.Chtimes(tmpPath, st.ModTime(), st.ModTime())
	}

	if err := os.Rename(tmpPath, dstFile); err != nil {
		return fmt.Errorf("renaming %s -> %s: %w", tmpPath, dstFile, err)
	}
	return nil
}

// AtomicCopyTree recursively installs all regular files from srcDir into dstDir using AtomicInstallFile.
func AtomicCopyTree(srcDir, dstDir string) error {
	root := filepath.Clean(srcDir)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dstDir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return AtomicInstallFile(path, target, info.Mode().Perm())
	})
}
