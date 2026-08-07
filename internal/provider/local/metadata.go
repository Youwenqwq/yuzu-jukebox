package local

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ffprobeMeta 是 `ffprobe -v quiet -print_format json -show_format -show_streams`
// 输出的相关子集。tags 键大小写随封装格式不定（如 MP4 常为大写），取值时折叠比较。
type ffprobeMeta struct {
	Format struct {
		Name    string            `json:"format_name"`
		Tags    map[string]string `json:"tags"`
		BitRate string            `json:"bit_rate"` // bps 字符串
	} `json:"format"`
	Streams []struct {
		CodecType   string         `json:"codec_type"`
		CodecName   string         `json:"codec_name"`
		Disposition map[string]int `json:"disposition"`
	} `json:"streams"`
}

var allowedAudioContainers = map[string]struct{}{
	"3g2": {}, "3gp": {}, "aac": {}, "ac3": {}, "adts": {}, "aiff": {},
	"amr": {}, "ape": {}, "asf": {}, "au": {}, "caf": {}, "dts": {},
	"eac3": {}, "flac": {}, "loas": {}, "m4a": {}, "matroska": {}, "mj2": {},
	"mov": {}, "mp3": {}, "mp4": {}, "mpeg": {}, "mpegts": {}, "ogg": {},
	"opus": {}, "tta": {}, "voc": {}, "wav": {}, "webm": {}, "wma": {}, "wv": {},
}

func isAllowedAudioContainer(formatName string) bool {
	for _, name := range strings.Split(strings.ToLower(formatName), ",") {
		if _, ok := allowedAudioContainers[strings.TrimSpace(name)]; ok {
			return true
		}
	}
	return false
}

// probeMeta 用一次 ffprobe 校验音频容器、提取专辑标签与整流码率，并在发现内嵌封面流
// （codec_type=video 且 disposition.attached_pic=1）时用 ffmpeg 抽出封面图。
// 容器探测失败或不在音频白名单中时拒绝上传；封面抽取失败仍是非致命的。
func (p *Provider) probeMeta(path, id string) (album string, bitrateKbps int, coverPath string, probeErr error) {
	out, err := exec.Command("ffprobe", "-v", "quiet",
		"-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		log.Printf("local: ffprobe meta %s: %v", path, err)
		return "", 0, "", fmt.Errorf("ffprobe metadata: %w", err)
	}
	var meta ffprobeMeta
	if err := json.Unmarshal(out, &meta); err != nil {
		log.Printf("local: parse ffprobe meta %s: %v", path, err)
		return "", 0, "", fmt.Errorf("parse ffprobe metadata: %w", err)
	}
	if !isAllowedAudioContainer(meta.Format.Name) {
		return "", 0, "", fmt.Errorf("unsupported media container %q", meta.Format.Name)
	}

	for k, v := range meta.Format.Tags {
		if strings.EqualFold(k, "album") {
			album = v
			break
		}
	}
	// bit_rate 为 bps 字符串；个别封装给出浮点，按浮点解析再换算。
	if f, err := strconv.ParseFloat(meta.Format.BitRate, 64); err == nil && f > 0 {
		bitrateKbps = int(f / 1000)
	}

	if codec := attachedPicCodec(meta); codec != "" {
		coverPath = p.extractCover(path, id, codec)
	}
	return album, bitrateKbps, coverPath, nil
}

// attachedPicCodec 返回内嵌封面流的 codec_name；无封面流返回空串。
func attachedPicCodec(meta ffprobeMeta) string {
	for _, s := range meta.Streams {
		if s.CodecType == "video" && s.Disposition["attached_pic"] == 1 {
			return s.CodecName
		}
	}
	return ""
}

// extractCover 用 ffmpeg 原样拷贝内嵌封面流到 {dir}/covers/{id}.{ext}，
// 扩展名按图像编码取（mjpeg→.jpg，png→.png，其余 .img）。失败返回空串。
func (p *Provider) extractCover(src, id, codec string) string {
	dir := filepath.Join(p.dir, "covers")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("local: mkdir covers: %v", err)
		return ""
	}
	ext := ".img"
	switch codec {
	case "mjpeg":
		ext = ".jpg"
	case "png":
		ext = ".png"
	}
	dst := filepath.Join(dir, id+ext)
	if err := exec.Command("ffmpeg", "-y", "-i", src, "-an", "-c:v", "copy", dst).Run(); err != nil {
		log.Printf("local: extract cover %s: %v", src, err)
		os.Remove(dst)
		return ""
	}
	return dst
}
