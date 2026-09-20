package pkgzip

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The store README documents these exact examples. Pinning them here catches a
// drift in the naming rules before a release is cut.
func TestArchiveNameMatchesStoreExamples(t *testing.T) {
	cases := map[string]string{
		ArchiveName("sample-provider", "0.1.0", "darwin", "arm64"): "sample-provider_0.1.0_darwin_arm64.zip",
		ArchiveName("sample-provider", "0.1.0", "linux", "amd64"):  "sample-provider_0.1.0_linux_amd64.zip",
		ArchiveName("local-llm-pool", "1.2.3", "windows", "amd64"): "local-llm-pool_1.2.3_windows_amd64.zip",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("archive name = %q, want %q", got, want)
		}
	}
}

func TestLibraryNameMatchesHostExtensions(t *testing.T) {
	cases := map[string]string{
		"linux":   "local-llm-pool.so",
		"darwin":  "local-llm-pool.dylib",
		"windows": "local-llm-pool.dll",
		"freebsd": "local-llm-pool.so",
	}
	for goos, want := range cases {
		if got := LibraryName("local-llm-pool", goos); got != want {
			t.Fatalf("library name for %s = %q, want %q", goos, got, want)
		}
	}
}

// packFixture builds one archive plus its checksums file in a temp directory.
func packFixture(t *testing.T, goos, goarch string, payload []byte) string {
	t.Helper()
	dir := t.TempDir()
	libPath := filepath.Join(dir, "built"+LibraryExtension(goos))
	if err := os.WriteFile(libPath, payload, 0o755); err != nil {
		t.Fatalf("write fixture library: %v", err)
	}
	out := filepath.Join(dir, "dist")
	if _, err := Pack("local-llm-pool", "1.2.3", goos, goarch, libPath, out); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := WriteChecksums(out); err != nil {
		t.Fatalf("WriteChecksums: %v", err)
	}
	return out
}

func TestPackProducesExactlyOneRootEntry(t *testing.T) {
	out := packFixture(t, "linux", "amd64", []byte("ELF-ish payload"))

	reader, err := zip.OpenReader(filepath.Join(out, "local-llm-pool_1.2.3_linux_amd64.zip"))
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) != 1 {
		t.Fatalf("entries = %d, want 1", len(reader.File))
	}
	entry := reader.File[0]
	if entry.Name != "local-llm-pool.so" {
		t.Fatalf("entry name = %q", entry.Name)
	}
	if strings.ContainsAny(entry.Name, `/\`) {
		t.Fatalf("entry %q is not at the archive root", entry.Name)
	}
	if entry.FileInfo().IsDir() || !entry.FileInfo().Mode().IsRegular() {
		t.Fatalf("entry mode = %v", entry.FileInfo().Mode())
	}
	if perm := entry.FileInfo().Mode().Perm(); perm != 0o755 {
		t.Fatalf("entry perm = %o, want 755", perm)
	}
}

func TestPackUsesPlatformExtension(t *testing.T) {
	for _, tc := range []struct{ goos, archive, entry string }{
		{"darwin", "local-llm-pool_1.2.3_darwin_arm64.zip", "local-llm-pool.dylib"},
		{"windows", "local-llm-pool_1.2.3_windows_arm64.zip", "local-llm-pool.dll"},
	} {
		out := packFixture(t, tc.goos, "arm64", []byte("payload"))
		reader, err := zip.OpenReader(filepath.Join(out, tc.archive))
		if err != nil {
			t.Fatalf("open %s: %v", tc.archive, err)
		}
		if len(reader.File) != 1 || reader.File[0].Name != tc.entry {
			t.Fatalf("%s holds %d entries, first %q", tc.archive, len(reader.File), reader.File[0].Name)
		}
		_ = reader.Close()
	}
}

func TestWriteChecksumsUsesSha256sumFormat(t *testing.T) {
	payload := []byte("deterministic payload")
	out := packFixture(t, "linux", "amd64", payload)

	raw, err := os.ReadFile(filepath.Join(out, ChecksumsFileName))
	if err != nil {
		t.Fatalf("read checksums: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("checksum lines = %d, want 1: %q", len(lines), raw)
	}
	name := "local-llm-pool_1.2.3_linux_amd64.zip"
	hash, rest, found := strings.Cut(lines[0], "  ")
	if !found || rest != name {
		t.Fatalf("line = %q, want '<sha256>  %s'", lines[0], name)
	}
	if len(hash) != sha256.Size*2 {
		t.Fatalf("hash length = %d", len(hash))
	}
	if _, errDecode := hex.DecodeString(hash); errDecode != nil {
		t.Fatalf("hash is not hex: %v", errDecode)
	}

	archive, errRead := os.ReadFile(filepath.Join(out, name))
	if errRead != nil {
		t.Fatalf("read archive: %v", errRead)
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != hash {
		t.Fatalf("recorded hash does not match the archive")
	}
}

func TestWriteChecksumsIsSortedAndCoversEveryArchive(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib.so")
	if err := os.WriteFile(lib, []byte("payload"), 0o755); err != nil {
		t.Fatalf("write library: %v", err)
	}
	out := filepath.Join(dir, "dist")
	for _, platform := range []struct{ goos, goarch string }{
		{"windows", "amd64"}, {"linux", "amd64"}, {"darwin", "arm64"},
	} {
		if _, err := Pack("local-llm-pool", "1.2.3", platform.goos, platform.goarch, lib, out); err != nil {
			t.Fatalf("Pack %s/%s: %v", platform.goos, platform.goarch, err)
		}
	}
	if _, err := WriteChecksums(out); err != nil {
		t.Fatalf("WriteChecksums: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(out, ChecksumsFileName))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	var names []string
	for _, line := range lines {
		names = append(names, strings.Fields(line)[1])
	}
	want := []string{
		"local-llm-pool_1.2.3_darwin_arm64.zip",
		"local-llm-pool_1.2.3_linux_amd64.zip",
		"local-llm-pool_1.2.3_windows_amd64.zip",
	}
	for index := range want {
		if names[index] != want[index] {
			t.Fatalf("checksums order = %v, want %v", names, want)
		}
	}

	if err := VerifyAll(out, "local-llm-pool", "1.2.3"); err != nil {
		t.Fatalf("VerifyAll: %v", err)
	}
}

func TestVerifyAcceptsAGoodArchive(t *testing.T) {
	out := packFixture(t, "linux", "amd64", []byte("payload"))
	if err := Verify(out, "local-llm-pool", "1.2.3", "linux", "amd64"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsBadArchives(t *testing.T) {
	name := "local-llm-pool_1.2.3_linux_amd64.zip"

	// A second entry, which the host rejects as multiple dynamic libraries.
	t.Run("two entries", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"local-llm-pool.so": "a", "extra.so": "b"})
		mustFailVerify(t, dir, "exactly one entry")
	})

	// A cgo header left in the archive is still a second entry.
	t.Run("stray header", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"local-llm-pool.so": "a", "local-llm-pool.h": "b"})
		mustFailVerify(t, dir, "exactly one entry")
	})

	// Nested, which readTargetLibrary rejects with "must be at zip root".
	t.Run("nested library", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"bin/local-llm-pool.so": "a"})
		mustFailVerify(t, dir, "archive root")
	})

	t.Run("wrong library name", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"plugin.so": "a"})
		mustFailVerify(t, dir, "want \"local-llm-pool.so\"")
	})

	t.Run("wrong extension for the platform", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"local-llm-pool.dylib": "a"})
		mustFailVerify(t, dir, "want \"local-llm-pool.so\"")
	})

	t.Run("empty library", func(t *testing.T) {
		dir := writeArchive(t, name, map[string]string{"local-llm-pool.so": ""})
		mustFailVerify(t, dir, "is empty")
	})

	t.Run("missing archive", func(t *testing.T) {
		mustFailVerify(t, t.TempDir(), "local-llm-pool_1.2.3_linux_amd64.zip")
	})
}

func TestVerifyRejectsAChecksumMismatch(t *testing.T) {
	out := packFixture(t, "linux", "amd64", []byte("payload"))
	path := filepath.Join(out, ChecksumsFileName)
	corrupted := strings.Repeat("0", 64) + "  local-llm-pool_1.2.3_linux_amd64.zip\n"
	if err := os.WriteFile(path, []byte(corrupted), 0o644); err != nil {
		t.Fatalf("rewrite checksums: %v", err)
	}
	err := Verify(out, "local-llm-pool", "1.2.3", "linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "records") {
		t.Fatalf("Verify error = %v, want a checksum mismatch", err)
	}
}

func TestVerifyRejectsAMissingChecksumLine(t *testing.T) {
	out := packFixture(t, "linux", "amd64", []byte("payload"))
	if err := os.WriteFile(filepath.Join(out, ChecksumsFileName), []byte(""), 0o644); err != nil {
		t.Fatalf("truncate checksums: %v", err)
	}
	err := Verify(out, "local-llm-pool", "1.2.3", "linux", "amd64")
	if err == nil || !strings.Contains(err.Error(), "no entry for") {
		t.Fatalf("Verify error = %v, want a missing entry", err)
	}
}

func TestReadChecksumsRejectsMalformedLines(t *testing.T) {
	for name, content := range map[string]string{
		"short hash":   "abc  file.zip\n",
		"not hex":      strings.Repeat("z", 64) + "  file.zip\n",
		"single field": strings.Repeat("a", 64) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ChecksumsFileName)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := ReadChecksums(path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPackRejectsAnEmptyLibrary(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "empty.so")
	if err := os.WriteFile(lib, nil, 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Pack("local-llm-pool", "1.2.3", "linux", "amd64", lib, filepath.Join(dir, "dist")); err == nil {
		t.Fatal("expected an error for an empty library")
	}
}

// writeArchive builds a deliberately shaped archive plus matching checksums so
// Verify fails on the archive layout rather than on the checksum.
func writeArchive(t *testing.T, archiveName string, entries map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, archiveName)
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	writer := zip.NewWriter(file)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	// Deterministic order so a multi-entry case always reports the same first entry.
	for index := 0; index < len(names); index++ {
		for other := index + 1; other < len(names); other++ {
			if names[other] < names[index] {
				names[index], names[other] = names[other], names[index]
			}
		}
	}
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o755)
		entry, errHeader := writer.CreateHeader(header)
		if errHeader != nil {
			t.Fatalf("create entry: %v", errHeader)
		}
		if _, errWrite := entry.Write([]byte(entries[name])); errWrite != nil {
			t.Fatalf("write entry: %v", errWrite)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}
	if _, err := WriteChecksums(dir); err != nil {
		t.Fatalf("WriteChecksums: %v", err)
	}
	return dir
}

func mustFailVerify(t *testing.T, dir, wantSubstring string) {
	t.Helper()
	err := Verify(dir, "local-llm-pool", "1.2.3", "linux", "amd64")
	if err == nil {
		t.Fatal("expected Verify to fail")
	}
	if !strings.Contains(err.Error(), wantSubstring) {
		t.Fatalf("Verify error = %q, want it to mention %q", err, wantSubstring)
	}
}
