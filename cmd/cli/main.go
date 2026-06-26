package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
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

const (
	interactiveStitchOption  = "1) Stitch videos (current capability)"
	interactivePrimaryOption = "2) Primary Horizontal"
	interactiveExitOption    = "3) Exit"
)

// ffprobe JSON structures
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  ffprobeFormat   `json:"format"`
}
type ffprobeFormat struct {
	Filename string            `json:"filename"`
	Duration string            `json:"duration"`
	Tags     map[string]string `json:"tags"`
}
type ffprobeStream struct {
	Index          int               `json:"index"`
	CodecName      string            `json:"codec_name"`
	CodecType      string            `json:"codec_type"`
	Width          int               `json:"width"`
	Height         int               `json:"height"`
	PixFmt         string            `json:"pix_fmt"`
	RFrameRate     string            `json:"r_frame_rate"`
	AvgFrameRate   string            `json:"avg_frame_rate"`
	SampleRate     string            `json:"sample_rate"`
	Channels       int               `json:"channels"`
	ChannelLayout  string            `json:"channel_layout"`
	ColorRange     string            `json:"color_range"`
	ColorSpace     string            `json:"colorspace"`
	ColorTransfer  string            `json:"color_transfer"`
	ColorPrimaries string            `json:"color_primaries"`
	Duration       string            `json:"duration"`
	Tags           map[string]string `json:"tags"`
	SideDataList   []ffprobeSideData `json:"side_data_list"`
}
type ffprobeSideData struct {
	Rotation float64 `json:"rotation"`
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

type PrimaryMediaKind int

const (
	primaryImage PrimaryMediaKind = iota
	primaryVideo
)

type PrimaryMedia struct {
	Path            string
	RenderPath      string
	Kind            PrimaryMediaKind
	Width           int
	Height          int
	HasAudio        bool
	DurationSeconds float64
	FPS             float64
	FPSExpr         string
	Timestamp       time.Time
	SizeBytes       int64
}

type PrimaryTarget struct {
	Width      int
	Height     int
	FPS        float64
	FPSExpr    string
	VideoCodec string
	Profile    string
	PixFmt     string
	Bitrate    string
	MaxRate    string
	BufSize    string
}

func main() {
	var (
		inputDir      string
		outputPath    string
		mute          bool
		forceFallback bool
		noPrompt      bool
		primaryMode   bool
	)
	flag.StringVar(&inputDir, "input", defaultInputDir, "Input folder (non-recursive)")
	flag.StringVar(&outputPath, "output", "", "Output file path (defaults to combined/<timestamp>.mp4)")
	flag.BoolVar(&mute, "mute", false, "Mute audio in the output")
	flag.BoolVar(&forceFallback, "force-fallback", false, "Force re-encode using Apple VideoToolbox instead of stream copy")
	flag.BoolVar(&noPrompt, "no-prompt", false, "Non-interactive mode (do not prompt)")
	flag.BoolVar(&primaryMode, "primary-horizontal", false, "Run Primary Horizontal mode for mixed photos/videos")
	flag.Parse()

	selectedOption := interactiveStitchOption
	if primaryMode {
		selectedOption = interactivePrimaryOption
	} else if !noPrompt {
		err := survey.AskOne(&survey.Select{
			Message: "Choose an option:",
			Options: []string{
				interactiveStitchOption,
				interactivePrimaryOption,
				interactiveExitOption,
			},
			Default: interactiveStitchOption,
		}, &selectedOption)
		must(err)
		if selectedOption == interactiveExitOption {
			fmt.Println("No action selected.")
			return
		}
	}

	switch selectedOption {
	case interactivePrimaryOption:
		runPrimaryHorizontalWorkflow(inputDir, outputPath, noPrompt)
	default:
		runStitchWorkflow(inputDir, outputPath, mute, forceFallback, noPrompt)
	}
}

func runStitchWorkflow(inputDir, outputPath string, mute, forceFallback, noPrompt bool) {
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

func runPrimaryHorizontalWorkflow(inputDir, outputPath string, noPrompt bool) {
	var err error
	if !noPrompt {
		err = survey.AskOne(&survey.Input{
			Message: "Media folder:",
			Default: inputDir,
		}, &inputDir, survey.WithValidator(validateNonEmpty("media folder cannot be empty")))
		must(err)
	}

	absInput, err := filepath.Abs(inputDir)
	must(err)

	ffmpegPath, ffprobePath, err := ensureFFBinaries()
	must(err)

	items, err := scanPrimaryMedia(ffprobePath, absInput)
	must(err)
	if len(items) == 0 {
		failf("No supported media found in: %s", absInput)
	}

	target, err := buildPrimaryTarget(items)
	must(err)

	if strings.TrimSpace(outputPath) == "" {
		outputPath = defaultPrimaryOutputPath(absInput)
	}
	if !noPrompt {
		err = survey.AskOne(&survey.Input{
			Message: "Output file path:",
			Default: outputPath,
		}, &outputPath, survey.WithValidator(validateNonEmpty("output path cannot be empty")))
		must(err)
	}
	outputPath, err = expandUser(outputPath)
	must(err)
	must(os.MkdirAll(filepath.Dir(outputPath), 0o755))

	fmt.Printf("Primary Horizontal plan:\n")
	fmt.Printf("- Media items: %d\n", len(items))
	fmt.Printf("- Target: %dx%d @ %s fps\n", target.Width, target.Height, target.FPSExpr)
	fmt.Printf("- Encoder: %s, profile=%s, pix_fmt=%s, bitrate=%s\n", target.VideoCodec, target.Profile, target.PixFmt, target.Bitrate)
	fmt.Printf("- Output: %s\n\n", outputPath)

	tmpDir, err := os.MkdirTemp(filepath.Dir(outputPath), ".primary_segments_*")
	must(err)
	defer func() { _ = os.RemoveAll(tmpDir) }()

	segments, err := buildPrimarySegments(ffmpegPath, items, target, tmpDir)
	must(err)

	listPath, cleanup, err := writeConcatFileList(segments)
	must(err)
	defer cleanup()

	fmt.Println("Combining normalized segments...")
	err = runFFmpegConcatCopy(ffmpegPath, listPath, outputPath)
	must(err)

	fmt.Printf("Primary Horizontal video written: %s\n", outputPath)
}

func validateNonEmpty(message string) survey.Validator {
	return func(ans interface{}) error {
		s, _ := ans.(string)
		if strings.TrimSpace(s) == "" {
			return errors.New(message)
		}
		return nil
	}
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

func scanPrimaryMedia(ffprobePath, dir string) ([]PrimaryMedia, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var candidates []string
	livePhotoVideoStems := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext == ".mov" {
			livePhotoVideoStems[strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))] = true
		}
		if isPrimaryImageExt(ext) || isPrimaryVideoExt(ext) {
			candidates = append(candidates, path)
		}
	}
	naturalSort(candidates)

	items := make([]PrimaryMedia, 0, len(candidates))
	for _, path := range candidates {
		ext := strings.ToLower(filepath.Ext(path))
		stem := strings.ToLower(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		if isPrimaryImageExt(ext) && livePhotoVideoStems[stem] {
			continue
		}
		item, err := probePrimaryMedia(ffprobePath, path)
		if err != nil {
			return nil, fmt.Errorf("probe failed on %s: %w", path, err)
		}
		if item.Width <= 0 || item.Height <= 0 {
			return nil, fmt.Errorf("missing dimensions for %s", path)
		}
		items = append(items, item)
	}

	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].Timestamp.Equal(items[j].Timestamp) {
			return items[i].Timestamp.Before(items[j].Timestamp)
		}
		return naturalLess(items[i].Path, items[j].Path)
	})
	return items, nil
}

func isPrimaryImageExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".heic", ".heif":
		return true
	default:
		return false
	}
}

func isPrimaryHEICExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".heic", ".heif":
		return true
	default:
		return false
	}
}

func isPrimaryVideoExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".mov", ".mp4", ".m4v":
		return true
	default:
		return false
	}
}

func probePrimaryMedia(ffprobePath, file string) (PrimaryMedia, error) {
	p, err := probeFile(ffprobePath, file)
	if err != nil {
		return PrimaryMedia{}, err
	}

	ext := strings.ToLower(filepath.Ext(file))
	item := PrimaryMedia{
		Path:      file,
		Timestamp: mediaTimestamp(p, file),
	}
	if st, err := os.Stat(file); err == nil {
		item.SizeBytes = st.Size()
	}
	if isPrimaryVideoExt(ext) {
		item.Kind = primaryVideo
	} else {
		item.Kind = primaryImage
		item.DurationSeconds = 3
	}

	if item.Kind == primaryImage && isPrimaryHEICExt(ext) {
		width, height, timestamp, err := sipsImageInfo(file)
		if err == nil {
			item.Width = width
			item.Height = height
			if !timestamp.IsZero() {
				item.Timestamp = timestamp
			}
		}
	}

	for _, s := range p.Streams {
		switch s.CodecType {
		case "video":
			if item.Width == 0 || item.Height == 0 {
				item.Width, item.Height = displayDimensions(s)
				item.FPSExpr = bestFrameRateExpr(s)
				item.FPS = parseFrameRate(item.FPSExpr)
				if item.Kind == primaryVideo {
					if d, err := strconv.ParseFloat(s.Duration, 64); err == nil && d > item.DurationSeconds {
						item.DurationSeconds = d
					}
				}
			}
		case "audio":
			item.HasAudio = true
		}
	}
	if item.Kind == primaryVideo {
		if d, err := strconv.ParseFloat(p.Format.Duration, 64); err == nil && d > 0 {
			item.DurationSeconds = d
		}
	}
	return item, nil
}

func sipsImageInfo(file string) (int, int, time.Time, error) {
	cmd := exec.Command("sips", "-g", "pixelWidth", "-g", "pixelHeight", "-g", "creation", file)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("sips probe failed: %w: %s", err, strings.TrimSpace(out.String()))
	}

	var width, height int
	var timestamp time.Time
	for _, line := range strings.Split(out.String(), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "pixelWidth":
			width, _ = strconv.Atoi(value)
		case "pixelHeight":
			height, _ = strconv.Atoi(value)
		case "creation":
			if ts, ok := parseMediaTimestamp(value); ok {
				timestamp = ts
			}
		}
	}
	if width <= 0 || height <= 0 {
		return 0, 0, time.Time{}, fmt.Errorf("sips did not report dimensions for %s", file)
	}
	return width, height, timestamp, nil
}

func displayDimensions(s ffprobeStream) (int, int) {
	w, h := s.Width, s.Height
	if rotationIsSideways(streamRotation(s)) {
		return h, w
	}
	return w, h
}

func streamRotation(s ffprobeStream) float64 {
	if raw := strings.TrimSpace(s.Tags["rotate"]); raw != "" {
		if rotation, err := strconv.ParseFloat(raw, 64); err == nil {
			return rotation
		}
	}
	for _, sideData := range s.SideDataList {
		if sideData.Rotation != 0 {
			return sideData.Rotation
		}
	}
	return 0
}

func rotationIsSideways(rotation float64) bool {
	normalized := int(math.Round(math.Abs(rotation))) % 360
	return normalized == 90 || normalized == 270
}

func defaultCombinedOutputPath(inputDir string) string {
	timestamp := time.Now().Format("20060102_150405")
	return filepath.Join(filepath.Dir(inputDir), defaultOutputDir, timestamp+defaultOutExt)
}

func defaultPrimaryOutputPath(inputDir string) string {
	timestamp := time.Now().Format("20060102_150405")
	return filepath.Join(inputDir, "processed", "primary_horizontal_"+timestamp+defaultOutExt)
}

func naturalSort(a []string) {
	re := regexp.MustCompile(`\d+|\D+`)
	sort.Slice(a, func(i, j int) bool {
		return naturalLessWithRegexp(re, a[i], a[j])
	})
}

func naturalLess(a, b string) bool {
	return naturalLessWithRegexp(regexp.MustCompile(`\d+|\D+`), a, b)
}

func naturalLessWithRegexp(re *regexp.Regexp, a, b string) bool {
	ai := re.FindAllString(a, -1)
	aj := re.FindAllString(b, -1)
	for k := 0; k < len(ai) && k < len(aj); k++ {
		if ai[k] == aj[k] {
			continue
		}
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

func probeFile(ffprobePath, file string) (ffprobeOutput, error) {
	args := []string{
		"-v", "quiet", "-print_format", "json",
		"-show_streams", "-show_format", file,
	}
	cmd := exec.Command(ffprobePath, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return ffprobeOutput{}, err
	}
	var p ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		return ffprobeOutput{}, err
	}
	return p, nil
}

func probeOne(ffprobePath, file string) (VideoInfo, error) {
	p, err := probeFile(ffprobePath, file)
	if err != nil {
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

func mediaTimestamp(p ffprobeOutput, file string) time.Time {
	for _, tags := range mediaTagSets(p) {
		for key, value := range tags {
			normalizedKey := strings.ToLower(strings.ReplaceAll(key, "_", ""))
			if strings.Contains(normalizedKey, "creation") || strings.Contains(normalizedKey, "date") {
				if ts, ok := parseMediaTimestamp(value); ok {
					return ts
				}
			}
		}
	}
	if st, err := os.Stat(file); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

func mediaTagSets(p ffprobeOutput) []map[string]string {
	var sets []map[string]string
	if len(p.Format.Tags) > 0 {
		sets = append(sets, p.Format.Tags)
	}
	for _, s := range p.Streams {
		if len(s.Tags) > 0 {
			sets = append(sets, s.Tags)
		}
	}
	return sets
}

func parseMediaTimestamp(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	formats := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.000000Z07:00",
		"2006-01-02T15:04:05.000000-0700",
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006:01:02 15:04:05",
		"2006:01:02 15:04:05-07:00",
		"2006:01:02 15:04:05-0700",
	}
	for _, format := range formats {
		if ts, err := time.Parse(format, value); err == nil {
			return ts, true
		}
		if ts, err := time.ParseInLocation(format, value, time.Local); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

func bestFrameRateExpr(s ffprobeStream) string {
	if fps := parseFrameRate(s.AvgFrameRate); fps > 0 {
		return s.AvgFrameRate
	}
	if fps := parseFrameRate(s.RFrameRate); fps > 0 {
		return s.RFrameRate
	}
	return ""
}

func parseFrameRate(expr string) float64 {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "0/0" {
		return 0
	}
	if strings.Contains(expr, "/") {
		parts := strings.SplitN(expr, "/", 2)
		num, nerr := strconv.ParseFloat(parts[0], 64)
		den, derr := strconv.ParseFloat(parts[1], 64)
		if nerr != nil || derr != nil || den == 0 {
			return 0
		}
		return num / den
	}
	fps, err := strconv.ParseFloat(expr, 64)
	if err != nil {
		return 0
	}
	return fps
}

func buildPrimaryTarget(items []PrimaryMedia) (PrimaryTarget, error) {
	if len(items) == 0 {
		return PrimaryTarget{}, errors.New("no media items")
	}

	var (
		preferredW    int
		preferredH    int
		preferredArea int
		fallbackW     int
		fallbackH     int
		fallbackArea  int
		bestFPS       float64
		bestExpr      string
	)
	for _, item := range items {
		w, h := item.Width, item.Height
		if w <= 0 || h <= 0 {
			continue
		}
		if h > w {
			w, h = h, w
		}
		area := w * h
		if !isPrimaryHEICExt(filepath.Ext(item.Path)) && (area > preferredArea || (area == preferredArea && w > preferredW)) {
			preferredW = w
			preferredH = h
			preferredArea = area
		}
		if area > fallbackArea || (area == fallbackArea && w > fallbackW) {
			fallbackW = w
			fallbackH = h
			fallbackArea = area
		}
		if item.Kind == primaryVideo && item.FPS > bestFPS {
			bestFPS = item.FPS
			bestExpr = item.FPSExpr
		}
	}
	targetW, targetH := preferredW, preferredH
	if targetW <= 0 {
		targetW, targetH = fallbackW, fallbackH
	}
	if targetW <= 0 || targetH <= 0 {
		return PrimaryTarget{}, errors.New("could not determine target resolution")
	}
	targetW = evenDimension(targetW)
	targetH = evenDimension(targetH)
	if bestFPS <= 0 {
		bestFPS = 30
		bestExpr = "30"
	}
	if bestExpr == "" {
		bestExpr = formatFPS(bestFPS)
	}
	target := PrimaryTarget{
		Width:      targetW,
		Height:     targetH,
		FPS:        bestFPS,
		FPSExpr:    bestExpr,
		VideoCodec: "hevc_videotoolbox",
		Profile:    "main10",
		PixFmt:     "p010le",
	}
	target.Bitrate, target.MaxRate, target.BufSize = primaryConstrainedBitrates(items, target)
	return target, nil
}

func primaryConstrainedBitrates(items []PrimaryMedia, target PrimaryTarget) (string, string, string) {
	sourceBytes := int64(0)
	for _, item := range items {
		sourceBytes += item.SizeBytes
	}
	duration := estimatePrimaryTimelineDuration(items, target)
	if sourceBytes <= 0 || duration <= 0 {
		return "16M", "20M", "32M"
	}

	maxOutputBits := float64(sourceBytes*3) * 8
	audioBits := duration * 192_000
	videoBitsPerSecond := ((maxOutputBits - audioBits) / duration) * 0.92
	megabits := videoBitsPerSecond / 1_000_000
	if megabits < 8 {
		megabits = 8
	}
	if megabits > 20 {
		megabits = 20
	}
	bitrate := int(math.Round(megabits))
	return fmt.Sprintf("%dM", bitrate), fmt.Sprintf("%dM", int(math.Round(megabits*1.25))), fmt.Sprintf("%dM", int(math.Round(megabits*2)))
}

func estimatePrimaryTimelineDuration(items []PrimaryMedia, target PrimaryTarget) float64 {
	duration := 0.0
	for i := 0; i < len(items); {
		item := items[i]
		if isPortraitPrimary(item) {
			groupSize := chooseVerticalGroupSize(items, i, target)
			duration += verticalGroupDuration(items[i : i+groupSize])
			i += groupSize
			continue
		}
		if item.Kind == primaryVideo && item.DurationSeconds > 0 {
			duration += item.DurationSeconds
		} else {
			duration += 3
		}
		i++
	}
	return duration
}

func evenDimension(n int) int {
	if n < 2 {
		return 2
	}
	if n%2 == 1 {
		return n - 1
	}
	return n
}

func formatFPS(fps float64) string {
	if math.Abs(fps-math.Round(fps)) < 0.001 {
		return strconv.Itoa(int(math.Round(fps)))
	}
	return strconv.FormatFloat(fps, 'f', 3, 64)
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

func buildPrimarySegments(ffmpegPath string, items []PrimaryMedia, target PrimaryTarget, tmpDir string) ([]string, error) {
	var segments []string
	for i := 0; i < len(items); {
		item := items[i]
		out := filepath.Join(tmpDir, fmt.Sprintf("segment_%05d.mp4", len(segments)+1))
		if isPortraitPrimary(item) {
			groupSize := chooseVerticalGroupSize(items, i, target)
			group := items[i : i+groupSize]
			i += groupSize
			preparedGroup, err := preparePrimaryRenderItems(group, tmpDir, len(segments)+1)
			if err != nil {
				return nil, err
			}
			fmt.Printf("Rendering vertical media group (%d): %s\n", len(group), primaryBasenames(group))
			if err := renderPrimaryVerticalMediaGroup(ffmpegPath, preparedGroup, target, out); err != nil {
				return nil, err
			}
			segments = append(segments, out)
			continue
		}

		fmt.Printf("Rendering: %s\n", filepath.Base(item.Path))
		preparedItem, err := preparePrimaryRenderItem(item, tmpDir, len(segments)+1, 0)
		if err != nil {
			return nil, err
		}
		var renderErr error
		switch preparedItem.Kind {
		case primaryImage:
			renderErr = renderPrimaryImageSegment(ffmpegPath, preparedItem, target, out)
		case primaryVideo:
			renderErr = renderPrimaryVideoSegment(ffmpegPath, preparedItem, target, out)
		default:
			renderErr = fmt.Errorf("unsupported media kind for %s", preparedItem.Path)
		}
		if renderErr != nil {
			return nil, renderErr
		}
		segments = append(segments, out)
		i++
	}
	return segments, nil
}

func preparePrimaryRenderItems(items []PrimaryMedia, tmpDir string, segmentIndex int) ([]PrimaryMedia, error) {
	prepared := make([]PrimaryMedia, len(items))
	for i, item := range items {
		renderItem, err := preparePrimaryRenderItem(item, tmpDir, segmentIndex, i)
		if err != nil {
			return nil, err
		}
		prepared[i] = renderItem
	}
	return prepared, nil
}

func preparePrimaryRenderItem(item PrimaryMedia, tmpDir string, segmentIndex, itemIndex int) (PrimaryMedia, error) {
	if item.Kind != primaryImage || !isPrimaryHEICExt(filepath.Ext(item.Path)) {
		return item, nil
	}

	output := filepath.Join(tmpDir, fmt.Sprintf("heic_%05d_%02d.png", segmentIndex, itemIndex+1))
	if err := convertHEICForRendering(item.Path, output); err != nil {
		return PrimaryMedia{}, err
	}
	item.RenderPath = output
	return item, nil
}

func convertHEICForRendering(input, output string) error {
	profile := "/System/Library/ColorSync/Profiles/ITU-709.icc"
	args := []string{"--matchTo", profile, "-s", "format", "png", input, "--out", output}
	cmd := exec.Command("sips", args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("HEIC conversion failed for %s: %w: %s", input, err, strings.TrimSpace(out.String()))
	}
	return nil
}

func primaryInputPath(item PrimaryMedia) string {
	if item.RenderPath != "" {
		return item.RenderPath
	}
	return item.Path
}

func isPortraitPrimary(item PrimaryMedia) bool {
	return item.Height > item.Width
}

func chooseVerticalGroupSize(items []PrimaryMedia, start int, target PrimaryTarget) int {
	remaining := 0
	for start+remaining < len(items) && remaining < 3 && isPortraitPrimary(items[start+remaining]) {
		remaining++
	}
	if remaining <= 2 {
		return remaining
	}
	score2 := verticalGroupOccupancy(items[start:start+2], target)
	score3 := verticalGroupOccupancy(items[start:start+3], target)
	if score3 > score2 {
		return 3
	}
	return 2
}

func verticalGroupOccupancy(group []PrimaryMedia, target PrimaryTarget) float64 {
	if len(group) == 0 {
		return 0
	}
	slotWidth := evenDimension(target.Width / len(group))
	total := 0
	for _, item := range group {
		w, h := fitDimensions(item.Width, item.Height, slotWidth, target.Height)
		total += w * h
	}
	return float64(total) / float64(target.Width*target.Height)
}

func primaryBasenames(items []PrimaryMedia) string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, filepath.Base(item.Path))
	}
	return strings.Join(names, ", ")
}

func renderPrimaryImageSegment(ffmpegPath string, item PrimaryMedia, target PrimaryTarget, output string) error {
	filter := primaryStillFitFilter(target)
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-i", primaryInputPath(item),
		"-f", "lavfi", "-t", "3", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
		"-filter_complex", filter,
		"-map", "[vout]", "-map", "1:a",
	}
	args = append(args, primaryVideoEncodeArgs(target)...)
	args = append(args, primaryAudioEncodeArgs()...)
	args = append(args, "-movflags", "+write_colr", "-y", output)
	return runLoggedCommand(ffmpegPath, args)
}

func renderPrimaryVerticalMediaGroup(ffmpegPath string, group []PrimaryMedia, target PrimaryTarget, output string) error {
	if len(group) == 0 || len(group) > 3 {
		return fmt.Errorf("vertical media group must contain 1 to 3 items, got %d", len(group))
	}

	args := []string{"-hide_banner", "-loglevel", "error"}
	for _, item := range group {
		args = append(args, "-i", primaryInputPath(item))
	}

	duration := verticalGroupDuration(group)
	filter, mapAudio, err := verticalMediaGroupFilter(group, target, duration)
	if err != nil {
		return err
	}
	if !mapAudio {
		args = append(args, "-f", "lavfi", "-t", formatDuration(duration), "-i", "anullsrc=channel_layout=stereo:sample_rate=48000")
	}
	args = append(args, "-filter_complex", filter, "-map", "[vout]")
	if mapAudio {
		args = append(args, "-map", "[aout]")
	} else {
		args = append(args, "-map", fmt.Sprintf("%d:a", len(group)))
	}
	args = append(args, primaryVideoEncodeArgs(target)...)
	args = append(args, primaryAudioEncodeArgs()...)
	args = append(args, "-movflags", "+write_colr", "-y", output)
	return runLoggedCommand(ffmpegPath, args)
}

func renderPrimaryVideoSegment(ffmpegPath string, item PrimaryMedia, target PrimaryTarget, output string) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-i", primaryInputPath(item)}
	filter := primaryFitFilter(target)
	if item.HasAudio {
		duration := item.DurationSeconds
		if duration <= 0 {
			duration = 3600
		}
		filter += fmt.Sprintf(";[0:a]aresample=48000,aformat=sample_rates=48000:channel_layouts=stereo,atrim=duration=%s,asetpts=PTS-STARTPTS[aout]", formatDuration(duration))
		args = append(args,
			"-filter_complex", filter,
			"-map", "[vout]", "-map", "[aout]",
		)
	} else {
		args = append(args,
			"-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000",
			"-filter_complex", filter,
			"-map", "[vout]", "-map", "1:a",
		)
	}
	args = append(args, primaryVideoEncodeArgs(target)...)
	args = append(args, primaryAudioEncodeArgs()...)
	args = append(args, "-movflags", "+write_colr", "-y", output)
	return runLoggedCommand(ffmpegPath, args)
}

func primaryFitFilter(target PrimaryTarget) string {
	return fmt.Sprintf("[0:v]scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,fps=%s,format=%s[vout]",
		target.Width, target.Height, target.Width, target.Height, target.FPSExpr, target.PixFmt)
}

func primaryStillFitFilter(target PrimaryTarget) string {
	return fmt.Sprintf("[0:v]tpad=stop_mode=clone:stop_duration=3,trim=duration=3,scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,setsar=1,fps=%s,format=%s[vout]",
		target.Width, target.Height, target.Width, target.Height, target.FPSExpr, target.PixFmt)
}

func verticalGroupDuration(group []PrimaryMedia) float64 {
	duration := 3.0
	for _, item := range group {
		if item.Kind == primaryVideo && item.DurationSeconds > duration {
			duration = item.DurationSeconds
		}
	}
	return duration
}

func formatDuration(duration float64) string {
	return strconv.FormatFloat(duration, 'f', 3, 64)
}

func verticalMediaGroupFilter(group []PrimaryMedia, target PrimaryTarget, duration float64) (string, bool, error) {
	var parts []string
	var labels []string
	slotWidth := evenDimension(target.Width / len(group))
	for i, item := range group {
		w, h := fitDimensions(item.Width, item.Height, slotWidth, target.Height)
		if w <= 0 || h <= 0 {
			return "", false, fmt.Errorf("could not fit %s into vertical group", item.Path)
		}
		label := fmt.Sprintf("slot%d", i)
		durationExpr := formatDuration(duration)
		if item.Kind == primaryImage {
			parts = append(parts, fmt.Sprintf("[%d:v]tpad=stop_mode=clone:stop_duration=%s,trim=duration=%s,setpts=PTS-STARTPTS,scale=%d:%d:flags=lanczos,setsar=1,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black[%s]", i, durationExpr, durationExpr, w, h, slotWidth, target.Height, label))
		} else {
			padDuration := math.Max(0, duration-item.DurationSeconds)
			parts = append(parts, fmt.Sprintf("[%d:v]tpad=stop_mode=clone:stop_duration=%s,trim=duration=%s,setpts=PTS-STARTPTS,scale=%d:%d:flags=lanczos,setsar=1,pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black[%s]", i, formatDuration(padDuration), durationExpr, w, h, slotWidth, target.Height, label))
		}
		labels = append(labels, fmt.Sprintf("[%s]", label))
	}
	stackLabel := "stacked"
	if len(group) == 1 {
		stackLabel = "slot0"
	} else {
		parts = append(parts, fmt.Sprintf("%shstack=inputs=%d[%s]", strings.Join(labels, ""), len(group), stackLabel))
	}
	parts = append(parts, fmt.Sprintf("[%s]pad=%d:%d:(ow-iw)/2:(oh-ih)/2:color=black,fps=%s,format=%s[vout]",
		stackLabel, target.Width, target.Height, target.FPSExpr, target.PixFmt))

	var audioLabels []string
	for i, item := range group {
		if !item.HasAudio {
			continue
		}
		label := fmt.Sprintf("a%d", i)
		parts = append(parts, fmt.Sprintf("[%d:a]aresample=48000,aformat=sample_rates=48000:channel_layouts=stereo,apad,atrim=duration=%s,asetpts=PTS-STARTPTS[%s]", i, formatDuration(duration), label))
		audioLabels = append(audioLabels, fmt.Sprintf("[%s]", label))
	}
	switch len(audioLabels) {
	case 0:
		return strings.Join(parts, ";"), false, nil
	case 1:
		parts = append(parts, fmt.Sprintf("%sanull[aout]", audioLabels[0]))
	default:
		parts = append(parts, fmt.Sprintf("%samix=inputs=%d:duration=longest:dropout_transition=0,volume=%0.4f[aout]", strings.Join(audioLabels, ""), len(audioLabels), 1.0/float64(len(audioLabels))))
	}
	return strings.Join(parts, ";"), true, nil
}

func fitDimensions(width, height, maxWidth, maxHeight int) (int, int) {
	if width <= 0 || height <= 0 || maxWidth <= 0 || maxHeight <= 0 {
		return 0, 0
	}
	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	w := evenDimension(int(math.Floor(float64(width) * scale)))
	h := evenDimension(int(math.Floor(float64(height) * scale)))
	return w, h
}

func primaryVideoEncodeArgs(target PrimaryTarget) []string {
	return []string{
		"-c:v", target.VideoCodec,
		"-profile:v", target.Profile,
		"-b:v", target.Bitrate,
		"-maxrate", target.MaxRate,
		"-bufsize", target.BufSize,
		"-pix_fmt", target.PixFmt,
		"-tag:v", "hvc1",
		"-colorspace", "bt709",
		"-color_trc", "bt709",
		"-color_primaries", "bt709",
		"-bsf:v", "hevc_metadata=colour_primaries=1:transfer_characteristics=1:matrix_coefficients=1",
	}
}

func primaryAudioEncodeArgs() []string {
	return []string{"-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2"}
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

func runFFmpegConcatCopy(ffmpegPath, fileList, output string) error {
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "concat", "-safe", "0",
		"-i", fileList,
		"-c", "copy",
		"-movflags", "+write_colr",
		"-y", output,
	}
	return runLoggedCommand(ffmpegPath, args)
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

func runLoggedCommand(name string, args []string) error {
	cmd := exec.Command(name, args...)
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
