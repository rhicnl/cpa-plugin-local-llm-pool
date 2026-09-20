// Package pkgzip builds and checks the release archives the CLIProxyAPI plugin
// store expects.
//
// The naming and layout rules mirror the host installer in
// internal/pluginstore of router-for-me/CLIProxyAPI:
//
//   - ArchiveName matches pluginstore.ArchiveName: <id>_<version>_<goos>_<goarch>.zip
//   - LibraryName matches pluginstore.pluginExtension: <id>.so | .dylib | .dll
//   - readTargetLibrary requires the library at the zip root, rejects nested
//     paths, and rejects more than one dynamic library per archive
//   - ParseChecksums reads sha256sum format: <64 hex chars><two spaces><name>
package pkgzip

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ChecksumsFileName is the fixed name the host looks for on a release.
const ChecksumsFileName = "checksums.txt"

// ArchiveName returns the release asset name for one platform.
func ArchiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", strings.TrimSpace(id), strings.TrimSpace(version), strings.TrimSpace(goos), strings.TrimSpace(goarch))
}

// LibraryName returns the single file the archive must contain at its root.
func LibraryName(id, goos string) string {
	return strings.TrimSpace(id) + LibraryExtension(goos)
}

// LibraryExtension returns the dynamic library suffix for goos.
func LibraryExtension(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

// Pack writes outDir/<archive name> containing exactly one entry, the library
// read from libPath, stored at the archive root under its required name.
func Pack(id, version, goos, goarch, libPath, outDir string) (string, error) {
	payload, errRead := os.ReadFile(libPath)
	if errRead != nil {
		return "", fmt.Errorf("read library: %w", errRead)
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("library %s is empty", libPath)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}

	archivePath := filepath.Join(outDir, ArchiveName(id, version, goos, goarch))
	file, errCreate := os.Create(archivePath)
	if errCreate != nil {
		return "", fmt.Errorf("create archive: %w", errCreate)
	}
	writer := zip.NewWriter(file)

	header := &zip.FileHeader{Name: LibraryName(id, goos), Method: zip.Deflate}
	// 0o755 so the host keeps an executable bit; it falls back to 0o755 only
	// when the entry reports mode 0.
	header.SetMode(0o755)
	entry, errHeader := writer.CreateHeader(header)
	if errHeader != nil {
		_ = writer.Close()
		_ = file.Close()
		return "", fmt.Errorf("create archive entry: %w", errHeader)
	}
	if _, errWrite := entry.Write(payload); errWrite != nil {
		_ = writer.Close()
		_ = file.Close()
		return "", fmt.Errorf("write archive entry: %w", errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		_ = file.Close()
		return "", fmt.Errorf("close archive: %w", errClose)
	}
	if errClose := file.Close(); errClose != nil {
		return "", fmt.Errorf("close archive file: %w", errClose)
	}
	return archivePath, nil
}

// WriteChecksums writes dir/checksums.txt covering every zip in dir, in
// sha256sum format and sorted by name so the file is reproducible.
func WriteChecksums(dir string) (string, error) {
	archives, errList := listArchives(dir)
	if errList != nil {
		return "", errList
	}
	if len(archives) == 0 {
		return "", fmt.Errorf("no .zip archives found in %s", dir)
	}

	var builder strings.Builder
	for _, name := range archives {
		sum, errSum := fileSHA256(filepath.Join(dir, name))
		if errSum != nil {
			return "", errSum
		}
		// Two spaces between hash and name is the sha256sum binary-mode-free
		// format the host's ParseChecksums reads.
		fmt.Fprintf(&builder, "%s  %s\n", sum, name)
	}
	path := filepath.Join(dir, ChecksumsFileName)
	if err := os.WriteFile(path, []byte(builder.String()), 0o644); err != nil {
		return "", fmt.Errorf("write checksums: %w", err)
	}
	return path, nil
}

// Verify re-checks one built archive the way the host installer would: the
// archive exists under its expected name, holds exactly one entry, that entry
// is a regular file at the root under the required library name, and
// checksums.txt carries a matching line for it.
func Verify(dir, id, version, goos, goarch string) error {
	archiveName := ArchiveName(id, version, goos, goarch)
	archivePath := filepath.Join(dir, archiveName)
	info, errStat := os.Stat(archivePath)
	if errStat != nil {
		return fmt.Errorf("archive %s: %w", archiveName, errStat)
	}

	reader, errOpen := zip.OpenReader(archivePath)
	if errOpen != nil {
		return fmt.Errorf("open %s: %w", archiveName, errOpen)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) != 1 {
		names := make([]string, 0, len(reader.File))
		for _, entry := range reader.File {
			names = append(names, entry.Name)
		}
		return fmt.Errorf("%s must contain exactly one entry, found %d: %v", archiveName, len(reader.File), names)
	}

	entry := reader.File[0]
	wantName := LibraryName(id, goos)
	if entry.Name != wantName {
		return fmt.Errorf("%s contains %q, want %q at the archive root", archiveName, entry.Name, wantName)
	}
	if strings.ContainsAny(entry.Name, `/\`) {
		return fmt.Errorf("%s entry %q is not at the archive root", archiveName, entry.Name)
	}
	if entry.FileInfo().IsDir() {
		return fmt.Errorf("%s entry %q is a directory", archiveName, entry.Name)
	}
	if !entry.FileInfo().Mode().IsRegular() {
		return fmt.Errorf("%s entry %q is not a regular file", archiveName, entry.Name)
	}
	if entry.UncompressedSize64 == 0 {
		return fmt.Errorf("%s entry %q is empty", archiveName, entry.Name)
	}

	checksums, errChecksums := ReadChecksums(filepath.Join(dir, ChecksumsFileName))
	if errChecksums != nil {
		return errChecksums
	}
	recorded, listed := checksums[archiveName]
	if !listed {
		return fmt.Errorf("%s has no entry for %s", ChecksumsFileName, archiveName)
	}
	actual, errSum := fileSHA256(archivePath)
	if errSum != nil {
		return errSum
	}
	if actual != recorded {
		return fmt.Errorf("%s records %s for %s, actual %s", ChecksumsFileName, recorded, archiveName, actual)
	}

	fmt.Printf("ok  %s  %d bytes, one entry %q, sha256 %s\n", archiveName, info.Size(), entry.Name, actual)
	return nil
}

// VerifyAll checks every archive listed in dir/checksums.txt, deriving the
// platform from each archive name. It is what the release job runs over the
// collected per-platform artifacts.
func VerifyAll(dir, id, version string) error {
	archives, errList := listArchives(dir)
	if errList != nil {
		return errList
	}
	if len(archives) == 0 {
		return fmt.Errorf("no .zip archives found in %s", dir)
	}
	prefix := strings.TrimSpace(id) + "_" + strings.TrimSpace(version) + "_"
	for _, name := range archives {
		rest, found := strings.CutPrefix(name, prefix)
		if !found {
			return fmt.Errorf("archive %s does not match the expected prefix %s", name, prefix)
		}
		platform := strings.TrimSuffix(rest, ".zip")
		goos, goarch, split := strings.Cut(platform, "_")
		if !split || goos == "" || goarch == "" {
			return fmt.Errorf("archive %s does not carry a <goos>_<goarch> suffix", name)
		}
		if err := Verify(dir, id, version, goos, goarch); err != nil {
			return err
		}
	}
	return nil
}

// ReadChecksums parses a sha256sum-format file into name to lowercase hash.
func ReadChecksums(path string) (map[string]string, error) {
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		return nil, fmt.Errorf("read checksums: %w", errRead)
	}
	out := make(map[string]string)
	for index, rawLine := range strings.Split(string(raw), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d: want '<sha256>  <filename>', got %q", filepath.Base(path), index+1, line)
		}
		hash := strings.ToLower(fields[0])
		if len(hash) != sha256.Size*2 {
			return nil, fmt.Errorf("%s line %d: sha256 must be %d hex characters", filepath.Base(path), index+1, sha256.Size*2)
		}
		if _, errDecode := hex.DecodeString(hash); errDecode != nil {
			return nil, fmt.Errorf("%s line %d: invalid sha256: %w", filepath.Base(path), index+1, errDecode)
		}
		out[fields[1]] = hash
	}
	return out, nil
}

func listArchives(dir string) ([]string, error) {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", dir, errRead)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".zip") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func fileSHA256(path string) (string, error) {
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return "", fmt.Errorf("open %s: %w", path, errOpen)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	if _, errCopy := io.Copy(digest, file); errCopy != nil {
		return "", fmt.Errorf("hash %s: %w", path, errCopy)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
