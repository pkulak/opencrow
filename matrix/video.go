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

const videoThumbnailFilter = "thumbnail=80,scale=640:640:force_original_aspect_ratio=decrease:force_divisible_by=2"

type videoMetadata struct {
	width      int
	height     int
	durationMS int
}

type videoThumbnail struct {
	data   []byte
	width  int
	height int
}

func (b *Backend) enrichVideoInfo(ctx context.Context, filePath string, info *event.FileInfo) {
	metadata, err := probeVideo(ctx, filePath)
	if err != nil {
		slog.Warn("failed to inspect video", "path", filePath, "error", err)
	} else {
		info.Width = metadata.width
		info.Height = metadata.height
		info.Duration = metadata.durationMS
	}

	thumbnail, err := generateVideoThumbnail(ctx, filePath)
	if err != nil {
		slog.Warn("failed to generate video thumbnail", "path", filePath, "error", err)

		return
	}

	baseName := strings.TrimSuffix(filepath.Base(filePath), filepath.Ext(filePath))

	resp, err := b.client.UploadMedia(ctx, mautrix.ReqUploadMedia{
		ContentBytes: thumbnail.data,
		ContentType:  "image/jpeg",
		FileName:     baseName + "-thumbnail.jpg",
	})
	if err != nil {
		slog.Warn("failed to upload video thumbnail", "path", filePath, "error", err)

		return
	}

	info.ThumbnailURL = resp.ContentURI.CUString()
	info.ThumbnailInfo = &event.FileInfo{
		MimeType: "image/jpeg",
		Size:     len(thumbnail.data),
		Width:    thumbnail.width,
		Height:   thumbnail.height,
	}
}

func probeVideo(ctx context.Context, filePath string) (videoMetadata, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height:format=duration",
		"-of", "json",
		filePath,
	)

	output, err := cmd.Output()
	if err != nil {
		return videoMetadata{}, mediaCommandError("ffprobe", err)
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
		return videoMetadata{}, fmt.Errorf("decoding ffprobe output: %w", err)
	}

	if len(result.Streams) == 0 || result.Streams[0].Width <= 0 || result.Streams[0].Height <= 0 {
		return videoMetadata{}, errors.New("ffprobe returned no video dimensions")
	}

	metadata := videoMetadata{
		width:  result.Streams[0].Width,
		height: result.Streams[0].Height,
	}

	if seconds, err := strconv.ParseFloat(result.Format.Duration, 64); err == nil && seconds > 0 {
		metadata.durationMS = int(math.Round(seconds * 1000))
	}

	return metadata, nil
}

func generateVideoThumbnail(ctx context.Context, filePath string) (videoThumbnail, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", filePath,
		"-vf", videoThumbnailFilter,
		"-frames:v", "1",
		"-an",
		"-c:v", "mjpeg",
		"-q:v", "3",
		"-f", "image2pipe",
		"pipe:1",
	)

	output, err := cmd.Output()
	if err != nil {
		return videoThumbnail{}, mediaCommandError("ffmpeg", err)
	}

	if len(output) == 0 {
		return videoThumbnail{}, errors.New("ffmpeg returned an empty thumbnail")
	}

	config, err := jpeg.DecodeConfig(bytes.NewReader(output))
	if err != nil {
		return videoThumbnail{}, fmt.Errorf("decoding generated thumbnail: %w", err)
	}

	return videoThumbnail{
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
