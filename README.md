# CLI Video Stitcher (Apple Silicon Optimized)

High-performance CLI video stitcher written in Go, optimized for Apple Silicon (M1/M2/M3, macOS arm64). It scans an input folder, analyzes formats using ffprobe, and concatenates MP4 files in serial order. It defaults to zero-reencode stream copy when the clips are uniform and falls back to Apple VideoToolbox hardware encoding when needed.

- Input folder (default): `/Volumes/macvault/100GOPRO` (non-recursive; handles spaces in path)
- Output file (default): `../combined/<timestamp>.mp4` relative to the input folder
- Audio: keep by default; optional mute toggle
- Preservation: keeps source resolution and encoding when possible via stream copy
- Auto-deps: downloads static ffmpeg and ffprobe for macOS arm64 on first run
- Fallback: uses VideoToolbox (h264_videotoolbox / hevc_videotoolbox) when stream copy is not possible

Source layout:
- [README.md](README.md)
- [go.mod](go.mod)
- [cmd/cli/main.go](cmd/cli/main.go)

## Features

- Fast-path concatenation via FFmpeg concat demuxer with stream copy:
  - `-f concat -safe 0 -c copy`
- Automatic detection of stream compatibility (codec, resolution, pixel format, fps/time base, audio layout)
- Hardware-accelerated fallback encode (VideoToolbox) normalizing to the majority profile when streams differ
- Audio options:
  - Keep audio streams (copy when compatible or AAC re-encode for fallback)
  - Mute (`-an`)
- Robust handling of file paths with spaces using concat file list quoting
- No global dependency setup required: fetches static macOS arm64 ffmpeg/ffprobe builds and caches them locally
- Interactive `2) Primary Horizontal` workflow for mixed `.jpg`, `.jpeg`, `.heic`, `.heif`, `.mov`, `.mp4`, and `.m4v` folders:
  - Orders media by timestamp metadata, with file modification time as fallback
  - Treats same-basename `.MOV` + still image pairs as Live Photos and uses the video
  - Creates `processed/primary_horizontal_<timestamp>.mp4` in the input folder by default
  - Uses the largest landscape resolution found in the folder and the highest video frame rate found
  - Shows still images for 3 seconds and chooses 2-vs-3 vertical grouping by actual screen occupancy without cropping
  - Plays grouped vertical videos/Live Photos side-by-side in parallel
  - Normalizes segments with Apple VideoToolbox HEVC Main10 (`hevc_videotoolbox`) using a source-size-aware bitrate targeting about 3x the source media size or less, then stream-copy concatenates them

## Requirements

- macOS arm64 (Apple Silicon)
- Go 1.22 or newer (for building the tool)
- Internet access on first run to download ffmpeg/ffprobe unless already on PATH

## Quickstart

1) Build the tool

```bash
go build -o bin/cli-videoeditor ./cmd/cli
```

2) Run with defaults

```bash
./bin/cli-videoeditor
```

This will:
- Show an interactive option menu:
  - `1) Stitch videos (current capability)` runs the original `.mp4` stitch workflow
  - `2) Primary Horizontal` creates a horizontal chronology video from mixed photos/videos
- Scan `/Volumes/macvault/100GOPRO` for `.mp4` files (non-recursive), natural-sort them by name
- Print an input summary (codec, resolution, fps, audio presence, total duration, uniformity)
- Write the result to `../combined/<timestamp>.mp4` relative to the input folder
- Attempt stream-copy concat; fallback to VideoToolbox encode if incompatible

3) Non-interactive usage

```bash
# Keep audio (default), use defaults for input/output
./bin/cli-videoeditor --no-prompt

# Explicit input folder with spaces and explicit output path
./bin/cli-videoeditor --no-prompt --input "./sept 3" --output "~/Movies/stitched.mp4"

# Mute audio
./bin/cli-videoeditor --no-prompt --mute

# Force fallback encode (skip stream copy)
./bin/cli-videoeditor --no-prompt --force-fallback

# Run Primary Horizontal on a mixed photo/video folder
./bin/cli-videoeditor --no-prompt --primary-horizontal --input "./camera roll"
```

Note: When using `--no-prompt`, ensure your output path exists or can be created. The tool will expand `~` automatically.

## CLI Flags

- `--input` string: Input directory (non-recursive). Default: `/Volumes/macvault/100GOPRO`
- `--output` string: Output file path. Default: `../combined/<timestamp>.mp4` relative to the input folder
- `--mute`: Mute audio in output (defaults to keeping audio)
- `--force-fallback`: Force hardware-accelerated re-encode instead of stream copy
- `--no-prompt`: Non-interactive run (no questions asked; uses provided/default values)
- `--primary-horizontal`: Run the mixed photo/video Primary Horizontal workflow

## How it works

1) Scan & Sort
- Scans the given input directory (non-recursive) for `.mp4`
- Natural sorting is applied so numerically suffixed names are ordered as expected

2) Probe
- Uses `ffprobe` (auto-downloaded if needed) to extract stream metadata for each file:
  - Video: codec, width, height, pixel format, frame rate
  - Audio: codec, sample rate, channels, channel layout
  - Duration
- Computes stream uniformity across all files (video/audio)

3) Plan
- If video and (when not muted) audio streams match across all inputs, use concat demuxer with `-c copy` (no re-encode)
- Otherwise, re-encode using VideoToolbox:
  - Video: `h264_videotoolbox` for H.264 inputs, `hevc_videotoolbox` for HEVC inputs
  - Pixel format: `yuv420p` for compatibility
  - Audio: copy when compatible; else AAC encode; or `-an` when muted

4) Execute
- Create a concat file list with absolute paths and proper quoting
- Run FFmpeg with either the stream-copy plan or the fallback encode plan
- Write output to the target path, creating directories if necessary

## Primary Horizontal Workflow

Choose `2) Primary Horizontal` from the interactive menu for folders that mix horizontal/vertical photos and videos.

- Scans the input folder non-recursively for `.jpg`, `.jpeg`, `.heic`, `.heif`, `.mov`, `.mp4`, and `.m4v`
- Sorts by media timestamp metadata (`creation_time` and common date tags), falling back to file modification time
- Skips still images when a same-basename `.MOV` exists, so Apple Live Photos are added as video
- Chooses a landscape target canvas from the highest-resolution media item in the folder
- Matches the output frame rate to the highest video frame rate found in the folder, defaulting to 30 fps when the folder contains only photos
- Renders still images for 3 seconds
- Places up to 3 consecutive vertical still images side-by-side in one horizontal frame by scaling each proportionally into equal slots
- Writes to `<input>/processed/primary_horizontal_<timestamp>.mp4` unless you enter a different output path
- Chooses whether 2 or 3 consecutive vertical items fit better by comparing actual visible pixel area in the horizontal frame
- Uses rotation metadata when classifying media, so rotated vertical Live Photo `.MOV` files are laid out as vertical media
- Plays grouped vertical videos and Live Photos in parallel in their own side-by-side tiles, preserving aspect ratio without cropping
- Encodes normalized segments with `hevc_videotoolbox`, HEVC Main10, explicit BT.709 color metadata, AAC audio, and then joins them with stream copy for Apple Silicon performance, strong compression, and better color fidelity than the original H.264 4:2:0 path

## Apple Silicon Optimization

- Prefers zero-reencode stream copy to avoid decode/encode costs when inputs are uniform
- For incompatible inputs, uses Apple VideoToolbox hardware acceleration:
  - `h264_videotoolbox` or `hevc_videotoolbox`
  - Preserves original resolution and fps to maintain source fidelity
- Primary Horizontal uses VideoToolbox HEVC Main10 for high-quality compressed segment rendering and stream-copy final concatenation
- Avoids unnecessary metadata writes and keeps the copy path direct

## Where ffmpeg/ffprobe come from

On first run (if not on PATH), the tool downloads static builds for macOS from:
- `evermeet.cx` mirrors for ffmpeg and ffprobe

They are cached at:
- `~/.cache/cli-videoeditor/bin/ffmpeg`
- `~/.cache/cli-videoeditor/bin/ffprobe`

Permissions are set automatically (`chmod +x`). If you prefer your own builds, place `ffmpeg` and `ffprobe` on your PATH, and the tool will use them instead.

## Troubleshooting

- No files found:
  - Ensure your input directory exists and contains `.mp4` files
  - Remember the default input is `./sept 3` (note the space). Quote your path if invoking via flags.

- Stream-copy fails:
  - The tool falls back automatically. You can force fallback with `--force-fallback`.

- Download errors for ffmpeg/ffprobe:
  - Check your internet connection or place `ffmpeg` and `ffprobe` in your PATH
  - Remove cached binaries in `~/.cache/cli-videoeditor/bin` and re-run to retry

- Permission issues:
  - Ensure the target output directory is writable. The tool creates the parent directory if needed.

## Build and Release

- Local build (Apple Silicon):

```bash
go build -o bin/cli-videoeditor ./cmd/cli
```

- Cross-build example (still on macOS arm64 target):

```bash
GOOS=darwin GOARCH=arm64 go build -o bin/cli-videoeditor ./cmd/cli
```

- Signing/Notarization:
  - If you plan to distribute the binary broadly, consider Apple code signing and notarization steps (not included here).

## Security Notes

- Downloaded ffmpeg/ffprobe come from public mirrors. If you require strict provenance:
  - Install ffmpeg/ffprobe via Homebrew or your own trusted build
  - Or modify the code to pin specific versions and verify checksums before use

## License

See [LICENSE](LICENSE).

## Acknowledgements

- FFmpeg project for the multimedia engine
- Evermeet for macOS static builds
- Apple VideoToolbox for accelerated transcoding

---

Implementation reference:
- Entry point and implementation: [cmd/cli/main.go](cmd/cli/main.go)
