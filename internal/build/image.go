package build

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// ImageInfo identifies the software that built a generation (manifest "image"). The Dockerfile
// bakes the build args into MAPTHING_IMAGE_* variables; the image ID itself is only known
// outside the container, so `docker run -e MAPTHING_IMAGE_ID=$(docker image inspect -f '{{.Id}}' …)`
// records it when the operator (or smoke.sh) passes it.
type ImageInfo struct {
	ID                string `json:"id,omitempty"`       // MAPTHING_IMAGE_ID (docker image ID/digest), if provided
	Version           string `json:"version,omitempty"`  // MAPTHING_IMAGE_VERSION
	Revision          string `json:"revision,omitempty"` // MAPTHING_IMAGE_REVISION (git commit)
	Created           string `json:"created,omitempty"`  // MAPTHING_IMAGE_CREATED
	Valhalla          string `json:"valhalla,omitempty"` // MAPTHING_VALHALLA_VERSION
	BasemapsRef       string `json:"basemaps_ref,omitempty"`
	PlanetilerJarSHA2 string `json:"planetiler_jar_sha256,omitempty"` // covers the profile patches
}

func imageInfo(t Tools) ImageInfo {
	return ImageInfo{
		ID:                os.Getenv("MAPTHING_IMAGE_ID"),
		Version:           os.Getenv("MAPTHING_IMAGE_VERSION"),
		Revision:          os.Getenv("MAPTHING_IMAGE_REVISION"),
		Created:           os.Getenv("MAPTHING_IMAGE_CREATED"),
		Valhalla:          os.Getenv("MAPTHING_VALHALLA_VERSION"),
		BasemapsRef:       os.Getenv("MAPTHING_BASEMAPS_REF"),
		PlanetilerJarSHA2: sha256File(t.PlanetilerJar),
	}
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
