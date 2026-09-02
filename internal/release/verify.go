package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// VerifyArchiveChecksum authenticates one archive against the checksum
// manifest and returns the packaged acr executable after validating its shape.
func VerifyArchiveChecksum(name string, archive, manifest []byte) ([]byte, error) {
	want, err := checksumFor(name, manifest)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(archive)
	got := hex.EncodeToString(digest[:])
	if got != want {
		return nil, fmt.Errorf("verify release archive %q: SHA-256 is %s, expected %s; discard the download and fetch the release assets again", name, got, want)
	}
	binary, err := validatedBinary(archive)
	if err != nil {
		return nil, fmt.Errorf("verify release archive %q: %w", name, err)
	}
	return binary, nil
}

func checksumFor(name string, manifest []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(manifest))
	seen := false
	want := ""
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 67 || line[64:66] != "  " {
			return "", fmt.Errorf("verify release checksums: malformed sha256sum line %q; download checksums.txt again", line)
		}
		digest, filename := line[:64], line[66:]
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(digest) != digest || filename == "" {
			return "", fmt.Errorf("verify release checksums: malformed sha256sum line %q; download checksums.txt again", line)
		}
		if filename == name {
			if seen {
				return "", fmt.Errorf("verify release checksums: archive %q appears more than once; discard the ambiguous manifest", name)
			}
			seen = true
			want = digest
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("verify release checksums: read manifest: %w; download checksums.txt again", err)
	}
	if !seen {
		return "", fmt.Errorf("verify release checksums: archive %q is absent; download the matching archive and checksums.txt from one release", name)
	}
	return want, nil
}

func validatedBinary(archive []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip stream: %w; discard the archive and download it again", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	seen := make(map[string]struct{}, 2)
	var binary []byte
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar stream: %w; discard the archive and download it again", err)
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("entry %q is not a regular file; discard the archive and download it again", header.Name)
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return nil, fmt.Errorf("entry %q appears more than once; discard the archive and download it again", header.Name)
		}
		seen[header.Name] = struct{}{}
		switch header.Name {
		case "acr":
			if header.Mode != 0o755 {
				return nil, fmt.Errorf("acr mode is %04o, expected 0755; discard the archive and download it again", header.Mode)
			}
			binary, err = io.ReadAll(tarReader)
			if err != nil {
				return nil, fmt.Errorf("read acr executable: %w; discard the archive and download it again", err)
			}
		case "LICENSE":
			if header.Mode != 0o644 {
				return nil, fmt.Errorf("LICENSE mode is %04o, expected 0644; discard the archive and download it again", header.Mode)
			}
			if _, err := io.Copy(io.Discard, tarReader); err != nil {
				return nil, fmt.Errorf("read LICENSE: %w; discard the archive and download it again", err)
			}
		default:
			return nil, fmt.Errorf("unexpected entry %q; discard the archive and download it again", header.Name)
		}
	}
	if len(seen) != 2 || len(binary) == 0 {
		return nil, fmt.Errorf("archive must contain non-empty acr and LICENSE files; discard the archive and download it again")
	}
	return binary, nil
}
