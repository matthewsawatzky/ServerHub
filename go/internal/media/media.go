// Package media stores user-supplied images (app logos and backgrounds) in a
// private media directory next to the rest of ServerHub's state.
package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"serverhub/internal/config"
	"serverhub/internal/storage"
)

const maxDownloadBytes = 10 << 20

// Dir returns the media directory, creating it if needed.
func Dir() (string, error) {
	base, err := storage.Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "media")
	if err := storage.EnsureDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// Path returns the on-disk path for a media file name and rejects traversal.
func Path(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("invalid media file")
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Save stores image bytes using the <prefix>-<id>.<ext> naming convention.
// hint is the original filename or URL, used to recover formats the sniffer
// does not recognise (notably WebP on older Go versions). allowJPEG lets
// backgrounds fall back to JPEG when ffmpeg lacks a WebP encoder.
func Save(prefix string, data []byte, hint string, allowJPEG bool) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty image")
	}
	ext := detectExt(data, hint)
	if ext == "" {
		return "", fmt.Errorf("unsupported image format")
	}
	if ext != ".svg" {
		if converted, ok := convertToWebP(data); ok {
			data = converted
			ext = ".webp"
		} else if allowJPEG && ext != ".jpg" && ext != ".jpeg" {
			if converted, ok := convertToJPEG(data); ok {
				data = converted
				ext = ".jpg"
			}
		}
	}
	idBytes := make([]byte, 4)
	if _, err := rand.Read(idBytes); err != nil {
		return "", err
	}
	base := config.Slugify(prefix)
	if base == "" {
		base = "image"
	}
	if len(base) > 48 {
		base = strings.TrimRight(base[:48], "-")
	}
	name := fmt.Sprintf("%s-%s%s", base, hex.EncodeToString(idBytes), ext)
	path, err := Path(name)
	if err != nil {
		return "", err
	}
	if err := storage.Write(path, data); err != nil {
		return "", err
	}
	return name, nil
}

// Download fetches an image URL and stores it with the given prefix.
func Download(ctx context.Context, rawURL, prefix string, allowJPEG bool) (string, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ServerHub")
	req.Header.Set("Accept", "image/avif,image/webp,image/svg+xml,image/*,*/*;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("image returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading image: %w", err)
	}
	if len(data) == 0 || len(data) > maxDownloadBytes {
		return "", fmt.Errorf("image must be between 1 byte and 10 MB")
	}
	name, err := Save(prefix, data, rawURL, allowJPEG)
	if err != nil {
		return "", err
	}
	_ = WriteMeta(name, Meta{SourceURL: rawURL, Kind: KindFromPrefix(prefix), CreatedAt: time.Now().Unix()})
	return name, nil
}

// List returns stored media filenames, optionally filtered by prefix.
func List(prefix string) ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if !isImageName(name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// Remove deletes a media file and its sidecar; missing files are ignored.
func Remove(name string) error {
	path, err := Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if meta, err := metaPath(name); err == nil {
		_ = os.Remove(meta)
	}
	return nil
}

// ContentType returns the MIME type for a stored media file.
func ContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	}
	return "application/octet-stream"
}

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg":
		return true
	}
	return false
}

func detectExt(data []byte, hint string) string {
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	if bytes.Contains(bytes.ToLower(head), []byte("<svg")) {
		return ".svg"
	}
	switch http.DetectContentType(data) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	switch strings.ToLower(filepath.Ext(hint)) {
	case ".png":
		return ".png"
	case ".jpg", ".jpeg":
		return ".jpg"
	case ".gif":
		return ".gif"
	case ".webp":
		return ".webp"
	case ".svg":
		return ".svg"
	}
	return ""
}

func convertToWebP(data []byte) ([]byte, bool) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, false
	}
	dir, err := Dir()
	if err != nil {
		return nil, false
	}
	in, err := os.CreateTemp(dir, ".convert-*")
	if err != nil {
		return nil, false
	}
	inName := in.Name()
	defer os.Remove(inName)
	if _, err := in.Write(data); err != nil {
		in.Close()
		return nil, false
	}
	if err := in.Close(); err != nil {
		return nil, false
	}
	outName := inName + ".webp"
	defer os.Remove(outName)
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", inName,
		"-frames:v", "1", "-vf", "scale='min(1600,iw)':-2", outName)
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	out, err := os.ReadFile(outName)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

func convertToJPEG(data []byte) ([]byte, bool) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, false
	}
	dir, err := Dir()
	if err != nil {
		return nil, false
	}
	in, err := os.CreateTemp(dir, ".convert-*")
	if err != nil {
		return nil, false
	}
	inName := in.Name()
	defer os.Remove(inName)
	if _, err := in.Write(data); err != nil {
		in.Close()
		return nil, false
	}
	if err := in.Close(); err != nil {
		return nil, false
	}
	outName := inName + ".jpg"
	defer os.Remove(outName)
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", inName,
		"-frames:v", "1", "-q:v", "3", "-vf", "scale='min(1600,iw)':-2", outName)
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	out, err := os.ReadFile(outName)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Exists reports whether a media file exists.
func Exists(name string) bool {
	path, err := Path(name)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// Meta is the small sidecar stored next to a media file.
type Meta struct {
	SourceURL string `json:"source_url,omitempty"`
	Kind      string `json:"kind,omitempty"`
	CreatedAt int64  `json:"created_at,omitempty"`
}

func metaPath(name string) (string, error) {
	path, err := Path(name)
	if err != nil {
		return "", err
	}
	return path + ".meta.json", nil
}

// ReadMeta returns a media file's sidecar, or an empty Meta when missing.
func ReadMeta(name string) Meta {
	path, err := metaPath(name)
	if err != nil {
		return Meta{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}
	}
	return meta
}

// WriteMeta saves a sidecar next to the media file.
func WriteMeta(name string, meta Meta) error {
	path, err := metaPath(name)
	if err != nil {
		return err
	}
	return storage.WriteJSON(path, meta)
}

// KindFromPrefix maps a storage prefix to a library category.
func KindFromPrefix(prefix string) string {
	switch prefix {
	case "background":
		return "background"
	case "doc":
		return "doc"
	case "image":
		return "image"
	}
	return "app"
}

// KindForName guesses a category when no sidecar exists.
func KindForName(name string) string {
	switch {
	case strings.HasPrefix(name, "background-"):
		return "background"
	case strings.HasPrefix(name, "doc-"):
		return "doc"
	}
	return "app"
}

// Copy duplicates a stored media file under a new prefix, preserving source
// metadata when present. Used when an app picks an image from the library.
func Copy(name, prefix string) (string, error) {
	sourcePath, err := Path(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return "", err
	}
	meta := ReadMeta(name)
	newName, err := Save(prefix, data, name, false)
	if err != nil {
		return "", err
	}
	meta.Kind = "app"
	if meta.CreatedAt == 0 {
		meta.CreatedAt = time.Now().Unix()
	}
	_ = WriteMeta(newName, meta)
	return newName, nil
}
