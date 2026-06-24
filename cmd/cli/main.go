package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
)

const (
	defaultInputDir  = "/Volumes/macvault/100GOPRO"
	defaultOutputDir = "combined"
	defaultOutExt    = ".mp4"
	appCacheSubdir   = ".cache/cli-videoeditor/bin"
	ffmpegZipName    = "ffmpeg.zip"
	ffprobeZipName   = "ffprobe.zip"
	ffmpegBinName    = "ffmpeg"
	ffprobeBinName   = "ffprobe"
	httpTimeout      = 60 * time.Second
)

// ffprobe JSON structures
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}
type ffprobeFormat struct {
	Filename string `json:"filename"`
	Duration string `json:"duration"`
}
type ffprobeStream struct {
	Index          int    `json:"index"`
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixFmt         string `json:"pix_fmt"`
	RFrameRate     string `json:"r_frame_rate"`
	SampleRate     string `json:"sample_rate"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	ColorRange     string `json:"color_range"`
	ColorSpace     string `json:"colorspace"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
}

type VideoInfo struct {
	Path            string
	HasVideo        bool
	VideoCodec      string
	Width           int
	Height          int
	PixFmt          string
	RFrameRate      string
	HasAudio        bool
	AudioCodec      string
	AudioSampleRate string
	AudioChannels   int
	AudioLayout     string
	DurationSeconds float64
}

type Uniformity struct {
	VideoUniform bool
	AudioUniform bool
	Reason       string
}

type EncodeTarget struct {
	VideoCodec      string
	Width           int
	Height          int
	PixFmt          string
	RFrameRate      string
	HasAudio        bool
	AudioCodec      string
	AudioSampleRate string
	AudioChannels   int
	AudioLayout     string
}

func main() {
	var (
		inputDir      string
		outputPath    string
		mute          bool
		forceFallback bool
		noPrompt      bool
	)
	flag.StringVar(&inputDir, "input", defaultInputDir, "Input folder (non-recursive)")
	flag.StringVar(&outputPath, "output", "", "Output file path (defaults to combined/<timestamp>.mp4)")
	flag.BoolVar(&mute, "mute", false, "Mute audio in the output")
	flag.BoolVar(&forceFallback, "force-fallback", false, "Force re-encode using Apple VideoToolbox instead of stream copy")
	flag.BoolVar(&noPrompt, "no-prompt", false, "Non-interactive mode (do not prompt)")
	flag.Parse()

	if strings.TrimSpace(outputPath) == "" {
		outputPath = defaultCombinedOutputPath(inputDir)
	}

	absInput, err := filepath.Abs(inputDir)
	must(err)

	ffmpegPath, ffprobePath, err := ensureFFBinaries()
	must(err)

	files, err := scanInputFiles(absInput)
	must(err)
	if len(files) == 0 {
		failf("No input files found in: %s", absInput)
	}

	fmt.Printf("Discovered %d files in: %s\n", len(files), absInput)
	for _, f := range files {
		fmt.Printf("  - %s\n", filepath.Base(f))
	}

	infos, uni, target := analyzeInputs(ffprobePath, files)
	printSummary(infos, uni, target)

	// Ask user for audio choice and output path if interactive.
	if !noPrompt {
		// Ask to keep audio or not; default keep
		keepAudio := !mute
		err = survey.AskOne(&survey.Confirm{
			Message: "Keep audio in stitched output?",
			Default: keepAudio,
		}, &keepAudio)
		must(err)
		mute = !keepAudio

		// Confirm or edit output path
		defOut := outputPath
		err = survey.AskOne(&survey.Input{
			Message: "Output file path:",
			Default: defOut,
		}, &outputPath, survey.WithValidator(func(ans interface{}) error {
			s, _ := ans.(string)
			if strings.TrimSpace(s) == "" {
				return errors.New("output path cannot be empty")
			}
			return nil
		}))
		must(err)
	}

	outputPath, err = expandUser(outputPath)
	must(err)
	must(os.MkdirAll(filepath.Dir(outputPath), 0o755))

	flistPath, cleanup, err := writeConcatFileList(files)
	must(err)
	defer cleanup()

	tryCopy := !forceFallback
	if tryCopy {
		// Only try zero-reencode copy when video and audio are uniform enough.
		// If user muted audio, video uniformity is still required for concat demuxer copy.
		if !uni.VideoUniform || (!mute && !uni.AudioUniform) {
			tryCopy = false
			fmt.Println("Streams not uniform; switching to hardware-accelerated normalize/encode.")
		}
	}

	if tryCopy {
		fmt.Println("Attempting zero-reencode stream copy...")
		err = runFFmpegCopy(ffmpegPath, flistPath, outputPath, mute)
		if err != nil {
			fmt.Printf("Stream copy failed: %v\n", err)
			fmt.Println("Falling back to Apple VideoToolbox hardware-accelerated encode...")
			tryCopy = false
		}
	}

	if !tryCopy {
		fmt.Println("Running hardware-accelerated normalize/encode via VideoToolbox...")
		err = runFFmpegEncode(ffmpegPath, files, outputPath, mute, target)
		must(err)
	}

	fmt.Printf("Stitched video written: %s\n", outputPath)
}

func must(err error) {
	if err != nil {
		failf("%v", err)
	}
}

func failf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}

func ensureFFBinaries() (string, string, error) {
	// Prefer system ffmpeg/ffprobe if available
	if p, _ := exec.LookPath(ffmpegBinName); p != "" {
		if q, _ := exec.LookPath(ffprobeBinName); q != "" {
			return p, q, nil
		}
	}

	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", "", errors.New("auto-download only supported on macOS arm64; please install ffmpeg/ffprobe")
	}

	cacheDir := filepath.Join(os.Getenv("HOME"), appCacheSubdir)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", "", err
	}
	ffmpegDst := filepath.Join(cacheDir, ffmpegBinName)
	ffprobeDst := filepath.Join(cacheDir, ffprobeBinName)

	// If already present, use them
	if fileExists(ffmpegDst) && fileExists(ffprobeDst) {
		return ffmpegDst, ffprobeDst, nil
	}

	fmt.Println("ffmpeg/ffprobe not found; downloading static macOS arm64 builds...")

	// Candidate URLs (attempt in order). These mirrors often provide universal/arm64 zips.
	ffmpegURLs := []string{
		"https://evermeet.cx/ffmpeg/getrelease/zip", // Latest release (universal)
		"https://evermeet.cx/ffmpeg/ffmpeg-6.1.zip",
		"https://evermeet.cx/ffmpeg/ffmpeg-6.0.zip",
	}
	ffprobeURLs := []string{
		"https://evermeet.cx/ffmpeg/getrelease/ffprobe/zip",
		"https://evermeet.cx/ffmpeg/ffprobe-6.1.zip",
		"https://evermeet.cx/ffmpeg/ffprobe-6.0.zip",
	}

	if !fileExists(ffmpegDst) {
		if err := downloadAndExtractSingleBinary(ffmpegURLs, ffmpegDst, ffmpegBinName); err != nil {
			return "", "", fmt.Errorf("failed to install ffmpeg: %w", err)
		}
	}
	if !fileExists(ffprobeDst) {
		if err := downloadAndExtractSingleBinary(ffprobeURLs, ffprobeDst, ffprobeBinName); err != nil {
			return "", "", fmt.Errorf("failed to install ffprobe: %w", err)
		}
	}

	return ffmpegDst, ffprobeDst, nil
}

func downloadAndExtractSingleBinary(urls []string, dstPath string, binName string) error {
	tmpZip := dstPath + ".zip"
	var lastErr error
	for _, u := range urls {
		fmt.Printf("  - Downloading %s...\n", u)
		if err := httpDownload(u, tmpZip); err != nil {
			lastErr = err
			continue
		}
		if err := unzipSingleBinary(tmpZip, dstPath, binName); err != nil {
			lastErr = err
			continue
		}
		_ = os.Remove(tmpZip)
		if err := os.Chmod(dstPath, 0o755); err != nil {
			lastErr = err
			continue
		}
		fmt.Printf("  Installed %s at %s\n", binName, dstPath)
		return nil
	}
	return lastErr
}

func httpDownload(url, dst string) error {
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("http status %s", resp.Status)
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, resp.Body)
	return err
}

func unzipSingleBinary(zipPath, dstPath, desiredName string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		name := filepath.Base(f.Name)
		if name == desiredName {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			data, err := io.ReadAll(rc)
			if err != nil {
				return err
			}
			return os.WriteFile(dstPath, data, 0o755)
		}
	}
	return fmt.Errorf("binary %s not found in zip", desiredName)
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func scanInputFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".mp4" {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	naturalSort(files)
	return files, nil
}

func defaultCombinedOutputPath(inputDir string) string {
	timestamp := time.Now().Format("20060102_150405")
	return filepath.Join(filepath.Dir(inputDir), defaultOutputDir, timestamp+defaultOutExt)
}

func naturalSort(a []string) {
	re := regexp.MustCompile(`\d+|\D+`)
	sort.Slice(a, func(i, j int) bool {
		ai := re.FindAllString(a[i], -1)
		aj := re.FindAllString(a[j], -1)
		for k := 0; k < len(ai) && k < len(aj); k++ {
			if ai[k] == aj[k] {
				continue
			}
			// Compare numeric tokens numerically
			ni, ei := strconv.Atoi(ai[k])
			nj, ej := strconv.Atoi(aj[k])
			if ei == nil && ej == nil {
				if ni != nj {
					return ni < nj
				}
			} else {
				return ai[k] < aj[k]
			}
		}
		return len(ai) < len(aj)
	})
}

func analyzeInputs(ffprobePath string, files []string) ([]VideoInfo, Uniformity, EncodeTarget) {
	infos := make([]VideoInfo, 0, len(files))
	videoCounts := map[string]int{}
	audioCounts := map[string]int{}
	videoSample := map[string]VideoInfo{}
	audioSample := map[string]VideoInfo{}
	allAudio := true
	allVideo := true
	for _, f := range files {
		inf, err := probeOne(ffprobePath, f)
		if err != nil {
			failf("ffprobe failed on %s: %v", f, err)
		}
		infos = append(infos, inf)
		if !inf.HasVideo {
			allVideo = false
		}
		if !inf.HasAudio {
			allAudio = false
		}
		vKey := videoSignature(inf)
		aKey := audioSignature(inf)
		videoCounts[vKey]++
		audioCounts[aKey]++
		if _, ok := videoSample[vKey]; !ok {
			videoSample[vKey] = inf
		}
		if _, ok := audioSample[aKey]; !ok {
			audioSample[aKey] = inf
		}
	}

	u := Uniformity{VideoUniform: true, AudioUniform: true}
	if len(infos) > 0 {
		ref := infos[0]
		for _, x := range infos[1:] {
			// Video uniform check
			if !(x.HasVideo == ref.HasVideo &&
				x.VideoCodec == ref.VideoCodec &&
				x.Width == ref.Width &&
				x.Height == ref.Height &&
				x.PixFmt == ref.PixFmt &&
				x.RFrameRate == ref.RFrameRate) {
				u.VideoUniform = false
			}
			// Audio uniform check
			if !(x.HasAudio == ref.HasAudio &&
				x.AudioCodec == ref.AudioCodec &&
				x.AudioSampleRate == ref.AudioSampleRate &&
				x.AudioChannels == ref.AudioChannels &&
				x.AudioLayout == ref.AudioLayout) {
				u.AudioUniform = false
			}
		}
	}
	if !u.VideoUniform || !u.AudioUniform {
		var reasons []string
		if !u.VideoUniform {
			reasons = append(reasons, "video streams mismatch")
		}
		if !u.AudioUniform {
			reasons = append(reasons, "audio streams mismatch")
		}
		u.Reason = strings.Join(reasons, "; ")
	}

	target := EncodeTarget{}
	if key, ok := mostCommonKey(videoCounts); ok {
		s := videoSample[key]
		target.VideoCodec = s.VideoCodec
		target.Width = s.Width
		target.Height = s.Height
		target.PixFmt = s.PixFmt
		target.RFrameRate = s.RFrameRate
	}
	if key, ok := mostCommonKey(audioCounts); ok {
		s := audioSample[key]
		target.HasAudio = allAudio && s.HasAudio
		target.AudioCodec = s.AudioCodec
		target.AudioSampleRate = s.AudioSampleRate
		target.AudioChannels = s.AudioChannels
		target.AudioLayout = s.AudioLayout
	}
	if !allVideo {
		target.VideoCodec = "h264"
	}
	return infos, u, target
}

func videoSignature(inf VideoInfo) string {
	return fmt.Sprintf("%t|%s|%dx%d|%s|%s", inf.HasVideo, inf.VideoCodec, inf.Width, inf.Height, inf.PixFmt, inf.RFrameRate)
}

func audioSignature(inf VideoInfo) string {
	return fmt.Sprintf("%t|%s|%s|%d|%s", inf.HasAudio, inf.AudioCodec, inf.AudioSampleRate, inf.AudioChannels, inf.AudioLayout)
}

func mostCommonKey(counts map[string]int) (string, bool) {
	var (
		bestKey   string
		bestCount int
		found     bool
	)
	for key, count := range counts {
		if !found || count > bestCount {
			bestKey = key
			bestCount = count
			found = true
		}
	}
	return bestKey, found
}

func probeOne(ffprobePath, file string) (VideoInfo, error) {
	args := []string{
		"-v", "quiet", "-print_format", "json",
		"-show_streams", "-show_format", file,
	}
	cmd := exec.Command(ffprobePath, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return VideoInfo{}, err
	}
	var p ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		return VideoInfo{}, err
	}

	vi := VideoInfo{Path: file}
	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			vi.HasVideo = true
			vi.VideoCodec = s.CodecName
			vi.Width = s.Width
			vi.Height = s.Height
			vi.PixFmt = s.PixFmt
			vi.RFrameRate = s.RFrameRate
		case "audio":
			vi.HasAudio = true
			vi.AudioCodec = s.CodecName
			vi.AudioSampleRate = s.SampleRate
			vi.AudioChannels = s.Channels
			vi.AudioLayout = s.ChannelLayout
		}
	}
	if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil {
		vi.DurationSeconds = d
	}
	return vi, nil
}

func printSummary(infos []VideoInfo, uni Uniformity, target EncodeTarget) {
	totalDur := 0.0
	vcodec := ""
	acodec := ""
	res := ""
	fps := ""
	audio := "no"
	if len(infos) > 0 {
		vcodec = infos[0].VideoCodec
		acodec = infos[0].AudioCodec
		res = fmt.Sprintf("%dx%d", infos[0].Width, infos[0].Height)
		fps = infos[0].RFrameRate
		if infos[0].HasAudio {
			audio = "yes"
		}
	}
	for _, x := range infos {
		totalDur += x.DurationSeconds
	}
	fmt.Printf("\nInput summary:\n")
	fmt.Printf("- Files: %d\n", len(infos))
	fmt.Printf("- Video: codec=%s, res=%s, fps=%s\n", vcodec, res, fps)
	fmt.Printf("- Audio present: %s, codec=%s\n", audio, acodec)
	fmt.Printf("- Total duration: %0.1fs\n", totalDur)
	fmt.Printf("- Uniform video streams: %v\n", uni.VideoUniform)
	fmt.Printf("- Uniform audio streams: %v\n", uni.AudioUniform)
	if target.VideoCodec != "" {
		fmt.Printf("- Majority video target: codec=%s, res=%dx%d, fps=%s\n", target.VideoCodec, target.Width, target.Height, target.RFrameRate)
	}
	if target.AudioCodec != "" {
		fmt.Printf("- Majority audio target: codec=%s, sample_rate=%s, channels=%d, layout=%s\n", target.AudioCodec, target.AudioSampleRate, target.AudioChannels, target.AudioLayout)
	}
	if uni.Reason != "" {
		fmt.Printf("- Non-uniform reason: %s\n", uni.Reason)
	}
	fmt.Println()
}

func writeConcatFileList(files []string) (string, func(), error) {
	tmp, err := os.CreateTemp("", "concat_list_*.txt")
	if err != nil {
		return "", func() {}, err
	}
	for _, f := range files {
		abs, _ := filepath.Abs(f)
		line := fmt.Sprintf("file '%s'\n", escapeSingleQuotes(abs))
		if _, err := tmp.WriteString(line); err != nil {
			tmp.Close()
			return "", func() {}, err
		}
	}
	if err := tmp.Close(); err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	return tmp.Name(), cleanup, nil
}

func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

func runFFmpegCopy(ffmpegPath, fileList, output string, mute bool) error {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0",
		"-i", fileList,
		"-c", "copy",
	}
	if mute {
		args = append(args, "-an")
	}
	args = append(args, "-y", output)
	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runFFmpegEncode(ffmpegPath string, files []string, output string, mute bool, target EncodeTarget) error {
	vCodec := "h264_videotoolbox"
	if codecLooksLikeHEVC(target.VideoCodec) {
		vCodec = "hevc_videotoolbox"
	}

	args := []string{"-hide_banner", "-loglevel", "error"}
	for _, f := range files {
		args = append(args, "-i", f)
	}

	filterComplex, mapArgs, includeAudio, err := buildNormalizeFilter(files, target, mute)
	if err != nil {
		return err
	}
	args = append(args, "-filter_complex", filterComplex)
	args = append(args, mapArgs...)
	args = append(args, "-c:v", vCodec, "-pix_fmt", "yuv420p")
	if vCodec == "hevc_videotoolbox" {
		args = append(args, "-tag:v", "hvc1")
	}
	if includeAudio {
		args = append(args, "-c:a", "aac", "-b:a", "192k")
	}
	args = append(args, "-y", output)

	cmd := exec.Command(ffmpegPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildNormalizeFilter(files []string, target EncodeTarget, mute bool) (string, []string, bool, error) {
	if len(files) == 0 {
		return "", nil, false, errors.New("no input files")
	}
	if target.Width == 0 || target.Height == 0 {
		return "", nil, false, errors.New("missing target video size")
	}

	var parts []string
	for i := range files {
		parts = append(parts, fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,fps=%s[v%d]", i, target.Width, target.Height, target.Width, target.Height, target.RFrameRate, i))
	}

	includeAudio := !mute && target.HasAudio && target.AudioSampleRate != "" && target.AudioChannels > 0 && target.AudioLayout != ""
	if includeAudio {
		for i := range files {
			parts = append(parts, fmt.Sprintf("[%d:a]aformat=sample_rates=%s:channel_layouts=%s,aresample=%s,asetpts=N/SR/TB[a%d]", i, target.AudioSampleRate, target.AudioLayout, target.AudioSampleRate, i))
		}
	}

	var concatInputs []string
	for i := range files {
		concatInputs = append(concatInputs, fmt.Sprintf("[v%d]", i))
		if includeAudio {
			concatInputs = append(concatInputs, fmt.Sprintf("[a%d]", i))
		}
	}
	if includeAudio {
		parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=1[vout][aout]", strings.Join(concatInputs, ""), len(files)))
	} else {
		parts = append(parts, fmt.Sprintf("%sconcat=n=%d:v=1:a=0[vout]", strings.Join(concatInputs, ""), len(files)))
	}

	mapArgs := []string{"-map", "[vout]"}
	if includeAudio {
		mapArgs = append(mapArgs, "-map", "[aout]")
	}
	return strings.Join(parts, ";"), mapArgs, includeAudio, nil
}

func expandUser(path string) (string, error) {
	if path == "~" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME not set")
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME not set")
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func codecLooksLikeHEVC(codec string) bool {
	switch strings.ToLower(codec) {
	case "hevc", "h265", "hev1", "hvc1":
		return true
	default:
		return false
	}
}
