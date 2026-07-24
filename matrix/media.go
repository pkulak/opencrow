package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"log/slog"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
)

const (
	thumbnailContentType = "image/jpeg"
	imageThumbnailFilter = "scale='min(640,iw)':'min(640,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2"
	videoThumbnailFilter = "thumbnail=80," + imageThumbnailFilter
)

type mediaMetadata struct {
	width      int
	height     int
	durationMS int
}

type mediaThumbnail struct {
	data   []byte
	width  int
	height int
}

func (b *Backend) enrichImageInfo(ctx context.Context, filePath string, info *event.FileInfo) {
	metadata, err := probeMedia(ctx, filePath)
	if err != nil {
		slog.Warn("failed to inspect image", "path", filePath, "error", err)
	} else {
		info.Width = metadata.width
		info.Height = metadata.height
	}

	b.enrichThumbnail(ctx, filePath, imageThumbnailFilter, info)
}

func (b *Backend) enrichVideoInfo(ctx context.Context, filePath string, info *event.FileInfo) {
	metadata, err := probeMedia(ctx, filePath)
	if err != nil {
		slog.Warn("failed to inspect video", "path", filePath, "error", err)
	} else {
		info.Width = metadata.width
		info.Height = metadata.height
		info.Duration = metadata.durationMS
	}

	b.enrichThumbnail(ctx, filePath, videoThumbnailFilter, info)
}

func (b *Backend) enrichThumbnail(ctx context.Context, filePath, filter string, info *event.FileInfo) {
	thumbnail, err := generateThumbnail(ctx, filePath, filter)
	if err != nil {
		slog.Warn("failed to generate media thumbnail", "path", filePath, "error", err)

		return
	}

	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	resp, err := b.client.UploadMedia(ctx, mautrix.ReqUploadMedia{
		ContentBytes: thumbnail.data,
		ContentType:  thumbnailContentType,
		FileName:     baseName + "-thumbnail.jpg",
	})
	if err != nil {
		slog.Warn("failed to upload media thumbnail", "path", filePath, "error", err)

		return
	}

	info.ThumbnailURL = resp.ContentURI.CUString()
	info.ThumbnailInfo = &event.FileInfo{
		MimeType: thumbnailContentType,
		Size:     len(thumbnail.data),
		Width:    thumbnail.width,
		Height:   thumbnail.height,
	}
}

func probeMedia(ctx context.Context, filePath string) (mediaMetadata, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
		"-of", "json",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return mediaMetadata{}, mediaCommandError("ffprobe", err)
	}

	var result struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return mediaMetadata{}, fmt.Errorf("decoding ffprobe output: %w", err)
	}

	if len(result.Streams) == 0 || result.Streams[0].Width <= 0 || result.Streams[0].Height <= 0 {
		return mediaMetadata{}, errors.New("ffprobe returned no media dimensions")
	}

	metadata := mediaMetadata{
		width:  result.Streams[0].Width,
		height: result.Streams[0].Height,
	}

	if seconds, err := strconv.ParseFloat(result.Format.Duration, 64); err == nil && seconds > 0 {
		metadata.durationMS = int(math.Round(seconds * 1000))
	}

	return metadata, nil
}

func generateThumbnail(ctx context.Context, filePath, filter string) (mediaThumbnail, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", filePath,
		"-vf", filter,
		"-frames:v", "1",
		"-an",
		"-c:v", "mjpeg",
		"-q:v", "3",
		"-f", "image2pipe",
		"pipe:1",
	)

	output, err := cmd.Output()
	if err != nil {
		return mediaThumbnail{}, mediaCommandError("ffmpeg", err)
	}

	if len(output) == 0 {
		return mediaThumbnail{}, errors.New("ffmpeg returned an empty thumbnail")
	}

	config, err := jpeg.DecodeConfig(bytes.NewReader(output))
	if err != nil {
		return mediaThumbnail{}, fmt.Errorf("decoding generated thumbnail: %w", err)
	}

	return mediaThumbnail{
		data:   output,
		width:  config.Width,
		height: config.Height,
	}, nil
}

func mediaCommandError(name string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
			return fmt.Errorf("running %s: %w: %s", name, err, stderr)
		}
	}

	return fmt.Errorf("running %s: %w", name, err)
}
