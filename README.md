# CLI Video Stitcher (Apple Silicon Optimized)

High-performance CLI video stitcher written in Go, optimized for Apple Silicon (M1/M2/M3, macOS arm64). It scans an input folder, analyzes formats using ffprobe, and concatenates MP4 files in serial order. It defaults to zero-reencode stream copy (fastest) and falls back to Apple VideoToolbox hardware encoding when needed. Prompts allow keeping or muting audio and setting the output file path.

- Input folder (default): `./sept 3` (non-recursive; handles spaces in path)
- Output file (default): `~/Movies/stitched.mp4`
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
  - `-f concat -safe 0 -c copy -movflags +faststart`
- Automatic detection of stream compatibility (codec, resolution, pixel format, fps/time base, audio layout)
- Hardware-accelerated fallback encode (VideoToolbox) preserving original resolution and fps
- Audio options:
  - Keep audio streams (copy when compatible or AAC re-encode for fallback)
  - Mute (`-an`)
- Robust handling of file paths with spaces using concat file list quoting
- No global dependency setup required: fetches static macOS arm64 ffmpeg/ffprobe builds and caches them locally

## Requirements

- macOS arm64 (Apple Silicon)
- Go 1.22 or newer (for building the tool)
- Internet access on first run to download ffmpeg/ffprobe unless already on PATH

## Quickstart

1) Build the tool

```bash
go build -o bin/cli-videoeditor ./cmd/cli
```

2) Run with defaults (interactive prompts on first run)

```bash
./bin/cli-videoeditor
```

This will:
- Scan `./sept 3` for `.mp4` files (non-recursive), natural-sort them by name
- Print an input summary (codec, resolution, fps, audio presence, total duration, uniformity)
- Prompt to keep audio (default yes)
- Prompt for output path (default `~/Movies/stitched.mp4`)
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
```

Note: When using `--no-prompt`, ensure your output path exists or can be created. The tool will expand `~` automatically.

## CLI Flags

- `--input` string: Input directory (non-recursive). Default: `./sept 3`
- `--output` string: Output file path. Default: `~/Movies/stitched.mp4`
- `--mute`: Mute audio in output (defaults to keeping audio)
- `--force-fallback`: Force hardware-accelerated re-encode instead of stream copy
- `--no-prompt`: Non-interactive run (no questions asked; uses provided/default values)

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
  - `-movflags +faststart` for better playback on web and Apple players
  - Audio: copy when compatible; else AAC encode; or `-an` when muted

4) Execute
- Create a concat file list with absolute paths and proper quoting
- Run FFmpeg with either the stream-copy plan or the fallback encode plan
- Write output to the target path, creating directories if necessary

## Apple Silicon Optimization

- Prefers zero-reencode stream copy to avoid decode/encode costs when inputs are uniform
- For incompatible inputs, uses Apple VideoToolbox hardware acceleration:
  - `h264_videotoolbox` or `hevc_videotoolbox`
  - Preserves original resolution and fps to maintain source fidelity
- Avoids unnecessary metadata writes and uses `+faststart` to optimize output playback

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