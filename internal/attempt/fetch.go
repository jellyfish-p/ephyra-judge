package attempt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	judgev1 "github.com/jellyfish-p/ephyra/protocol/judge/v1"
)

func (r *Runner) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return http.DefaultClient
}

func (r *Runner) fetchData(ctx context.Context, files []*judgev1.FileRef) (map[string][]byte, string) {
	out := make(map[string][]byte, len(files))
	for _, f := range files {
		if f == nil {
			return nil, faultMissingFile
		}
		key, kind := dataKey(f.GetName())
		if kind != "" {
			return nil, kind
		}
		body, kind := r.fetch(ctx, f)
		if kind != "" {
			return nil, kind
		}
		out[key] = body
	}
	return out, ""
}

func (r *Runner) fetch(ctx context.Context, ref *judgev1.FileRef) ([]byte, string) {
	if ref == nil {
		return nil, faultMissingFile
	}
	if ref.GetSizeBytes() > maxFileBytes {
		return nil, faultFileTooLarge
	}
	claimed := strings.TrimSpace(ref.GetSha256Hex())
	if !validSHA256Hex(claimed) {
		return nil, faultHashMismatch
	}
	cachePath := filepath.Join(r.WorkDir, "objects", strings.ToLower(claimed))
	if cached, err := os.ReadFile(cachePath); err == nil {
		if kind := verifyBlob(cached, ref); kind == "" {
			return cached, ""
		}
	}
	if ref.GetDownloadUrl() == "" {
		return nil, faultDownloadFailed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.GetDownloadUrl(), nil)
	if err != nil {
		return nil, faultDownloadFailed
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, faultDownloadFailed
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, faultDownloadFailed
	}
	limit := ref.GetSizeBytes()
	if limit <= 0 {
		limit = maxFileBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, faultDownloadFailed
	}
	if kind := verifyBlob(body, ref); kind != "" {
		return nil, kind
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err == nil {
		_ = os.WriteFile(cachePath, body, 0o644)
	}
	return body, ""
}

func verifyBlob(body []byte, ref *judgev1.FileRef) string {
	if int64(len(body)) != ref.GetSizeBytes() {
		return faultSizeMismatch
	}
	sum := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(ref.GetSha256Hex())) {
		return faultHashMismatch
	}
	return ""
}

func validSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func dataKey(name string) (string, string) {
	if strings.TrimSpace(name) == "" {
		return "", faultMissingFileName
	}
	if strings.Contains(name, "..") {
		return "", faultIllegalPath
	}
	base := baseFileName(name)
	if base == "." || base == "/" || base == "" {
		return "", faultIllegalPath
	}
	return base, ""
}

func baseFileName(name string) string {
	return path.Base(strings.ReplaceAll(name, "\\", "/"))
}

func shaHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
