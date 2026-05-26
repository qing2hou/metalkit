// realdeps.go is the production wiring of the installer interfaces. It
// is compiled into the agent binary; unit tests in this package use
// hand-rolled mocks instead so this file only needs to be exercised by
// integration tests against a real live boot image.
//
// Worth flagging:
//
//   - HTTPDownloader's sha256VerifyReader returns the hash mismatch as an
//     io.Read error AFTER the underlying body returns EOF. qemu-img will
//     surface this as a failed convert and the install will fail at the
//     write stage rather than silently flashing corrupt bytes.
//   - LsblkDiskLister tolerates lsblk versions that emit RM/RO as either
//     boolean or numeric "0"/"1" by funneling them through a small
//     flexBool type.
package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ---- OSExec ---------------------------------------------------------------

// OSExec runs commands via os/exec.CommandContext. Combined output is
// returned so callers can include it in their error messages.
type OSExec struct{}

// Run executes name with args. Returns the combined stdout+stderr buffer.
// On non-zero exit the returned error message includes the captured
// output so the agent can ship the failing-command diagnostics to the
// controller in a single line.
func (OSExec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w (output: %s)",
			name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// RunPipe streams stdin into the command. Stdout+stderr are captured
// and folded into the returned error on non-zero exit. The stdin reader
// is consumed until EOF; cmd.Wait blocks until the child closes its end.
func (OSExec) RunPipe(ctx context.Context, stdin io.Reader, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w (output: %s)",
			name, strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return nil
}

// ---- OSFS -----------------------------------------------------------------

// OSFS implements FS over the host filesystem.
type OSFS struct{}

func (OSFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (OSFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (OSFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (OSFS) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (OSFS) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ---- HTTPDownloader -------------------------------------------------------

// HTTPDownloader streams image blobs over HTTP with on-the-fly sha256
// verification. The expectedSHA256 is a 64-char hex string; passing an
// empty string is allowed (skip verification) but the agent always sets
// it from spec.ImageSHA256 which is required upstream.
type HTTPDownloader struct {
	Client *http.Client
}

// Stream issues a GET to url and returns a ReadCloser that yields the
// body bytes while accumulating a sha256. On EOF the final hash is
// compared to expectedSHA256; a mismatch is surfaced as a Read error so
// callers can't accidentally consume a corrupt stream successfully.
func (h HTTPDownloader) Stream(ctx context.Context, url, expectedSHA256 string) (io.ReadCloser, error) {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("install: build GET %s: %w", url, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("install: GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("install: GET %s: status %d", url, resp.StatusCode)
	}
	return &sha256VerifyReader{
		body:     resp.Body,
		expected: strings.ToLower(strings.TrimSpace(expectedSHA256)),
		hasher:   sha256.New(),
	}, nil
}

// sha256VerifyReader wraps an http.Response.Body. On EOF from the wrapped
// reader it returns an error if the running sha256 doesn't match the
// expected value, otherwise it forwards the EOF.
type sha256VerifyReader struct {
	body     io.ReadCloser
	expected string
	hasher   hash.Hash
	done     bool
}

func (r *sha256VerifyReader) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.hasher.Write(p[:n])
	}
	if err == io.EOF && !r.done {
		r.done = true
		if r.expected == "" {
			return n, io.EOF
		}
		got := hex.EncodeToString(r.hasher.Sum(nil))
		if got != r.expected {
			return n, fmt.Errorf("install: sha256 mismatch: got %s want %s", got, r.expected)
		}
	}
	return n, err
}

func (r *sha256VerifyReader) Close() error {
	return r.body.Close()
}

// ---- LsblkDiskLister ------------------------------------------------------

// LsblkDiskLister enumerates block devices via lsblk -J. The JSON form
// is stable across distro versions; we tolerate RM/RO being either
// boolean true/false or numeric "0"/"1" via flexBool.
type LsblkDiskLister struct {
	Exec Exec // injected so unit tests of the lister itself can mock out
}

// List runs lsblk and returns the parsed Disk slice.
func (l LsblkDiskLister) List(ctx context.Context) ([]Disk, error) {
	exe := l.Exec
	if exe == nil {
		exe = OSExec{}
	}
	out, err := exe.Run(ctx, "lsblk", "-J", "-d", "-bo",
		"NAME,PATH,SIZE,RM,RO,TRAN,MODEL,WWN")
	if err != nil {
		return nil, fmt.Errorf("install: lsblk: %w", err)
	}
	var parsed struct {
		BlockDevices []struct {
			Name      string   `json:"name"`
			Path      string   `json:"path"`
			Size      int64    `json:"size"`
			RM        flexBool `json:"rm"`
			RO        flexBool `json:"ro"`
			Tran      string   `json:"tran"`
			Model     string   `json:"model"`
			WWN       string   `json:"wwn"`
		} `json:"blockdevices"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("install: parse lsblk output: %w", err)
	}
	disks := make([]Disk, 0, len(parsed.BlockDevices))
	for _, d := range parsed.BlockDevices {
		path := d.Path
		if path == "" {
			path = "/dev/" + d.Name
		}
		disks = append(disks, Disk{
			Name:      d.Name,
			DevPath:   path,
			SizeBytes: d.Size,
			Removable: bool(d.RM),
			ReadOnly:  bool(d.RO),
			Transport: d.Tran,
			Model:     strings.TrimSpace(d.Model),
			WWN:       strings.TrimSpace(d.WWN),
		})
	}
	return disks, nil
}

// flexBool unmarshals true/false, 0/1, "0"/"1", "true"/"false". Older
// lsblk versions emit RM/RO as a string; newer ones as a bool.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	switch s {
	case "true", `"true"`, "1", `"1"`:
		*b = true
		return nil
	case "false", `"false"`, "0", `"0"`, "null":
		*b = false
		return nil
	}
	// Try generic bool/int as a fallback.
	var bv bool
	if err := json.Unmarshal(data, &bv); err == nil {
		*b = flexBool(bv)
		return nil
	}
	var iv int
	if err := json.Unmarshal(data, &iv); err == nil {
		*b = flexBool(iv != 0)
		return nil
	}
	// Strip quotes and try integer parsing.
	stripped := strings.Trim(s, `"`)
	if n, err := strconv.Atoi(stripped); err == nil {
		*b = flexBool(n != 0)
		return nil
	}
	return fmt.Errorf("install: cannot parse %q as bool", s)
}
