// Package local 实现本地上传媒体的 Provider。
// 媒体文件落在配置的 media 目录，元数据存于 media_files 表。
package local

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/youwenqwq/yuzu-jukebox/internal/provider"
	"github.com/youwenqwq/yuzu-jukebox/internal/store"
)

var (
	ErrInvalidRef = errors.New("invalid local media ref")
	ErrNotFound   = errors.New("local media not found")
)

type Provider struct {
	dir string
	st  *store.Store
}

func New(dir string, st *store.Store) *Provider { return &Provider{dir: dir, st: st} }

func (p *Provider) ID() string { return "local" }

// Add 把上传内容落盘并登记元数据。durationMs <= 0 时按文件内容探测。
func (p *Provider) Add(ctx context.Context, filename string, r io.Reader, title, artist, uploadedBy string, durationMs int64) (provider.Track, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	id := newID()
	dst := filepath.Join(p.dir, id+ext)

	f, err := os.Create(dst)
	if err != nil {
		return provider.Track{}, err
	}
	size, err := io.Copy(f, r)
	cerr := f.Close()
	if err != nil {
		os.Remove(dst)
		return provider.Track{}, err
	}
	if cerr != nil {
		os.Remove(dst)
		return provider.Track{}, cerr
	}

	if durationMs <= 0 {
		durationMs, err = probeDuration(dst, ext)
		if err != nil {
			os.Remove(dst)
			return provider.Track{}, fmt.Errorf("duration unknown: %w (pass duration_ms explicitly)", err)
		}
	}
	if title == "" {
		title = strings.TrimSuffix(filename, ext)
	}

	album, bitrateKbps, coverPath := p.probeMeta(dst, id)

	m := store.MediaFile{
		ID: id, Filename: id + ext, Title: title, Artist: artist,
		DurationMs: durationMs, SizeBytes: size, UploadedBy: uploadedBy,
		Album: album, CoverPath: coverPath, BitrateKbps: bitrateKbps,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := p.st.AddMediaFile(ctx, m); err != nil {
		os.Remove(dst)
		return provider.Track{}, err
	}
	return p.trackOf(m), nil
}

func (p *Provider) Search(ctx context.Context, query string, limit, offset int) ([]provider.Track, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	files, err := p.st.SearchMediaFiles(ctx, query, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]provider.Track, 0, len(files))
	for _, m := range files {
		out = append(out, p.trackOf(m))
	}
	return out, nil
}

func (p *Provider) List(ctx context.Context) ([]store.MediaFile, error) {
	return p.st.ListMediaFiles(ctx)
}

// Delete 先删除数据库行，再尽力删除磁盘文件，避免留下指向缺失文件的行。
func (p *Provider) Delete(ctx context.Context, ref provider.TrackRef) error {
	m, err := p.mediaOf(ctx, ref)
	if err != nil {
		return err
	}
	if err := p.st.DeleteMediaFile(ctx, m.ID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if err := os.Remove(filepath.Join(p.dir, m.Filename)); err != nil {
		log.Printf("local: remove media %s: %v", ref, err)
	}
	return nil
}

func (p *Provider) GetTrack(ctx context.Context, ref provider.TrackRef) (provider.Track, error) {
	m, err := p.mediaOf(ctx, ref)
	if err != nil {
		return provider.Track{}, err
	}
	return p.trackOf(m), nil
}

// Resolve 对本地文件直接返回 file:// 定位符，缓存层会短路。
func (p *Provider) Resolve(ctx context.Context, ref provider.TrackRef) (provider.StreamLocator, error) {
	m, err := p.mediaOf(ctx, ref)
	if err != nil {
		return provider.StreamLocator{}, err
	}
	path := filepath.Join(p.dir, m.Filename)
	if _, err := os.Stat(path); err != nil {
		return provider.StreamLocator{}, fmt.Errorf("media file missing: %w", err)
	}
	return provider.StreamLocator{
		URL:         "file://" + path,
		Format:      strings.TrimPrefix(strings.ToLower(filepath.Ext(m.Filename)), "."),
		DurationMs:  m.DurationMs,
		SizeBytes:   m.SizeBytes,
		BitrateKbps: m.BitrateKbps,
	}, nil
}

func (p *Provider) mediaOf(ctx context.Context, ref provider.TrackRef) (store.MediaFile, error) {
	providerID, id, err := ref.Split()
	if err != nil || providerID != p.ID() {
		return store.MediaFile{}, ErrInvalidRef
	}
	m, err := p.st.GetMediaFile(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.MediaFile{}, ErrNotFound
		}
		return store.MediaFile{}, err
	}
	return m, nil
}

func (p *Provider) trackOf(m store.MediaFile) provider.Track {
	t := provider.Track{
		Ref:        provider.NewRef(p.ID(), m.ID),
		Title:      m.Title,
		Artist:     m.Artist,
		DurationMs: m.DurationMs,
		Album:      m.Album,
	}
	if m.CoverPath != "" {
		// 原始值即磁盘上的封面文件路径；assembly 层重写为 /api/v1/cover/{ref}。
		t.CoverURL = m.CoverPath
	}
	if m.UploadedBy != "" {
		t.Contributors = []provider.Contributor{{Role: "uploader", Name: m.UploadedBy}}
	}
	return t
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
