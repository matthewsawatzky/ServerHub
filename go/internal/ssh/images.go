package ssh

import (
	"context"
	"encoding/json"
	"strings"
)

// Image is one local Docker image (repository:tag) as reported by `docker
// images`. Repository may be "<none>" for dangling images.
type Image struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	ID         string `json:"id"`
	Size       string `json:"size"`
	Created    string `json:"created"`
}

// ImageList is a timestamped snapshot of a host's Docker images.
type ImageList struct {
	UpdatedAt int64   `json:"updated_at"`
	Images    []Image `json:"images"`
}

const dockerImagesCmd = `docker images --format '{"repository":"{{.Repository}}","tag":"{{.Tag}}","id":"{{.ID}}","size":"{{.Size}}","created":"{{.CreatedSince}}"}'`

// ParseImageLines turns `docker images` JSON lines into sanitized Image values.
// Remote-controlled fields are cleaned and length-bounded at the boundary.
func ParseImageLines(out string) []Image {
	var images []Image
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var img Image
		if err := json.Unmarshal([]byte(line), &img); err != nil {
			continue
		}
		img.Repository = cleanText(img.Repository, 255)
		img.Tag = cleanText(img.Tag, 128)
		img.ID = cleanText(img.ID, 128)
		img.Size = cleanText(img.Size, 64)
		img.Created = cleanText(img.Created, 64)
		if SanitizeImage(img) {
			images = append(images, img)
		}
	}
	return images
}

// SanitizeImage rejects empty or over-long identities. Field values are cleaned
// by ParseImageLines; the frontend still escapes them.
func SanitizeImage(img Image) bool {
	if img.ID == "" || len(img.ID) > 128 || len(img.Repository) > 255 || len(img.Tag) > 128 {
		return false
	}
	for _, s := range []string{img.Repository, img.Tag, img.Size, img.Created} {
		if strings.ContainsAny(s, "\x00\r\n") {
			return false
		}
	}
	return true
}

// ValidContainerName reports whether name is safe to pass to a Docker command.
func ValidContainerName(name string) bool {
	return SanitizeContainer(Container{Name: name})
}

// CollectImages fetches the Docker image list over the shared connection.
// A missing Docker binary is treated as an empty list.
func (c *Conn) CollectImages(ctx context.Context) ([]Image, error) {
	out, err := c.RunCommand(ctx, dockerImagesCmd)
	if err != nil {
		return nil, nil
	}
	return ParseImageLines(out), nil
}
