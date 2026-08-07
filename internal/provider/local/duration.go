package local

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// probeDuration 探测媒体时长（毫秒）。
// WAV 直接解析头；其他格式依赖系统 ffprobe。
func probeDuration(path, ext string) (int64, error) {
	if ext == ".wav" {
		if ms, err := wavDurationMs(path); err == nil {
			return ms, nil
		}
	}
	return ffprobeDurationMs(path)
}

// wavDurationMs 解析 PCM WAV 的 RIFF 头计算时长。
func wavDurationMs(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return 0, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, errors.New("not a RIFF/WAVE file")
	}

	var byteRate uint32
	var dataSize uint32
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return 0, errors.New("missing data chunk")
		}
		id := string(hdr[0:4])
		size := binary.LittleEndian.Uint32(hdr[4:8])
		switch id {
		case "fmt ":
			if size > 64<<20 {
				return 0, errors.New("fmt chunk too large")
			}
			offset, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return 0, err
			}
			if remaining := info.Size() - offset; remaining < 0 || int64(size) > remaining {
				return 0, errors.New("fmt chunk exceeds file size")
			}
			buf := make([]byte, size)
			if _, err := io.ReadFull(f, buf); err != nil {
				return 0, err
			}
			if size < 12 {
				return 0, errors.New("fmt chunk too small")
			}
			byteRate = binary.LittleEndian.Uint32(buf[8:12])
		case "data":
			dataSize = size
		default:
			if _, err := f.Seek(int64(size), io.SeekCurrent); err != nil {
				return 0, err
			}
		}
		if byteRate > 0 && dataSize > 0 {
			return int64(dataSize) * 1000 / int64(byteRate), nil
		}
	}
}

// ffprobeDurationMs 调用 ffprobe 获取时长（毫秒）。
func ffprobeDurationMs(path string) (int64, error) {
	out, err := exec.Command("ffprobe", "-v", "quiet",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe: %w", err)
	}
	s := strings.TrimSpace(string(out))
	sec, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("ffprobe output %q: %w", s, err)
	}
	return int64(sec * 1000), nil
}
