package firecracker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"firedoze/internal/store"
)

var ErrInvalidSnapshotBundle = errors.New("invalid snapshot bundle")
var ErrUnsupportedSnapshotExport = errors.New("unsupported snapshot export")

const (
	snapshotBundleFormat       = "firedoze.snapshot.v1"
	snapshotBundleManifestName = "manifest.json"
	snapshotBundleDiskName     = "rootfs.ext4"
)

type snapshotBundleManifest struct {
	Format            string          `json:"format"`
	Name              string          `json:"name"`
	SourceVM          string          `json:"source_vm,omitempty"`
	BaseImageID       string          `json:"base_image_id,omitempty"`
	KernelID          string          `json:"kernel_id,omitempty"`
	BaseImageMetadata json.RawMessage `json:"base_image_metadata,omitempty"`
	Disk              string          `json:"disk"`
	DiskSize          int64           `json:"disk_size"`
	CreatedAt         string          `json:"created_at,omitempty"`
}

func (m *Manager) ExportSnapshot(ctx context.Context, name string, out io.Writer) error {
	if name == "" {
		return errors.New("snapshot name is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	snapshot, err := m.store.GetSnapshot(ctx, name)
	if err != nil {
		return err
	}
	if snapshot.StatePath != "" || snapshot.MemPath != "" {
		return fmt.Errorf("%w: only disk snapshots can be exported", ErrUnsupportedSnapshotExport)
	}
	diskInfo, err := os.Stat(snapshot.DiskPath)
	if err != nil {
		return fmt.Errorf("snapshot disk: %w", err)
	}
	if !diskInfo.Mode().IsRegular() {
		return fmt.Errorf("snapshot disk is not a regular file: %s", snapshot.DiskPath)
	}

	manifest := snapshotBundleManifest{
		Format:            snapshotBundleFormat,
		Name:              snapshot.Name,
		SourceVM:          snapshot.SourceVM,
		BaseImageID:       snapshot.BaseImageID,
		KernelID:          snapshot.KernelID,
		BaseImageMetadata: snapshotMetadataJSON(snapshot.BaseImageMetadata),
		Disk:              snapshotBundleDiskName,
		DiskSize:          diskInfo.Size(),
		CreatedAt:         snapshot.CreatedAt,
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')

	disk, err := os.Open(snapshot.DiskPath)
	if err != nil {
		return err
	}
	defer disk.Close()

	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)

	now := time.Now()
	if err := writeTarFile(ctx, tw, snapshotBundleManifestName, int64(len(manifestData)), now, bytes.NewReader(manifestData)); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}

	if err := writeTarFile(ctx, tw, snapshotBundleDiskName, diskInfo.Size(), diskInfo.ModTime(), disk); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

func (m *Manager) ImportSnapshot(ctx context.Context, name string, in io.Reader) (store.Snapshot, error) {
	if name == "" {
		return store.Snapshot{}, errors.New("snapshot name is required")
	}
	exists, err := m.store.SnapshotExists(ctx, name)
	if err != nil {
		return store.Snapshot{}, err
	}
	if exists {
		return store.Snapshot{}, fmt.Errorf("%w: snapshot %q", ErrAlreadyExists, name)
	}

	layout := m.snapshotLayout(name)
	if _, err := os.Stat(layout.dir); err == nil {
		return store.Snapshot{}, fmt.Errorf("%w: snapshot directory %q", ErrAlreadyExists, layout.dir)
	} else if !os.IsNotExist(err) {
		return store.Snapshot{}, err
	}
	if err := os.MkdirAll(filepath.Dir(layout.dir), 0o755); err != nil {
		return store.Snapshot{}, err
	}
	tmpDir, err := os.MkdirTemp(filepath.Dir(layout.dir), "."+name+".import-*")
	if err != nil {
		return store.Snapshot{}, err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	manifest, err := unpackSnapshotBundle(ctx, in, filepath.Join(tmpDir, snapshotBundleDiskName))
	if err != nil {
		return store.Snapshot{}, err
	}
	if err := os.Rename(tmpDir, layout.dir); err != nil {
		return store.Snapshot{}, err
	}
	cleanupTmp = false

	metadata := ""
	if len(manifest.BaseImageMetadata) > 0 && string(manifest.BaseImageMetadata) != "null" {
		metadata = string(manifest.BaseImageMetadata)
	}
	snapshot, err := m.store.CreateSnapshot(ctx, store.CreateSnapshotParams{
		Name:              name,
		SourceVM:          manifest.SourceVM,
		DiskPath:          layout.diskPath,
		BaseImageID:       manifest.BaseImageID,
		KernelID:          manifest.KernelID,
		BaseImageMetadata: metadata,
	})
	if err != nil {
		_ = os.RemoveAll(layout.dir)
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return store.Snapshot{}, fmt.Errorf("%w: snapshot %q", ErrAlreadyExists, name)
		}
		return store.Snapshot{}, err
	}
	return snapshot, nil
}

func unpackSnapshotBundle(ctx context.Context, in io.Reader, diskPath string) (snapshotBundleManifest, error) {
	gz, err := gzip.NewReader(in)
	if err != nil {
		return snapshotBundleManifest{}, fmt.Errorf("%w: read gzip stream: %v", ErrInvalidSnapshotBundle, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var manifest snapshotBundleManifest
	manifestSeen := false
	diskSeen := false

	for {
		select {
		case <-ctx.Done():
			return snapshotBundleManifest{}, ctx.Err()
		default:
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return snapshotBundleManifest{}, fmt.Errorf("%w: read tar entry: %v", ErrInvalidSnapshotBundle, err)
		}
		name, err := cleanBundlePath(header.Name)
		if err != nil {
			return snapshotBundleManifest{}, err
		}
		if header.FileInfo().IsDir() {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return snapshotBundleManifest{}, fmt.Errorf("%w: unsupported tar entry %q", ErrInvalidSnapshotBundle, header.Name)
		}

		switch name {
		case snapshotBundleManifestName:
			if manifestSeen {
				return snapshotBundleManifest{}, fmt.Errorf("%w: duplicate manifest", ErrInvalidSnapshotBundle)
			}
			if header.Size > 1024*1024 {
				return snapshotBundleManifest{}, fmt.Errorf("%w: manifest is too large", ErrInvalidSnapshotBundle)
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return snapshotBundleManifest{}, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return snapshotBundleManifest{}, fmt.Errorf("%w: parse manifest: %v", ErrInvalidSnapshotBundle, err)
			}
			manifestSeen = true
		case snapshotBundleDiskName:
			if diskSeen {
				return snapshotBundleManifest{}, fmt.Errorf("%w: duplicate disk", ErrInvalidSnapshotBundle)
			}
			out, err := os.OpenFile(diskPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				return snapshotBundleManifest{}, err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return snapshotBundleManifest{}, copyErr
			}
			if closeErr != nil {
				return snapshotBundleManifest{}, closeErr
			}
			diskSeen = true
		default:
			return snapshotBundleManifest{}, fmt.Errorf("%w: unexpected file %q", ErrInvalidSnapshotBundle, header.Name)
		}
	}
	if !manifestSeen {
		return snapshotBundleManifest{}, fmt.Errorf("%w: missing manifest", ErrInvalidSnapshotBundle)
	}
	if manifest.Format != snapshotBundleFormat {
		return snapshotBundleManifest{}, fmt.Errorf("%w: unsupported format %q", ErrInvalidSnapshotBundle, manifest.Format)
	}
	if manifest.Disk != snapshotBundleDiskName {
		return snapshotBundleManifest{}, fmt.Errorf("%w: manifest disk must be %q", ErrInvalidSnapshotBundle, snapshotBundleDiskName)
	}
	if !diskSeen {
		return snapshotBundleManifest{}, fmt.Errorf("%w: missing disk", ErrInvalidSnapshotBundle)
	}
	diskInfo, err := os.Stat(diskPath)
	if err != nil {
		return snapshotBundleManifest{}, err
	}
	if manifest.DiskSize != 0 && diskInfo.Size() != manifest.DiskSize {
		return snapshotBundleManifest{}, fmt.Errorf("%w: disk size mismatch", ErrInvalidSnapshotBundle)
	}
	return manifest, nil
}

func writeTarFile(ctx context.Context, tw *tar.Writer, name string, size int64, modTime time.Time, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o644,
		Size:    size,
		ModTime: modTime,
	}); err != nil {
		return err
	}
	_, err := io.Copy(tw, readerWithContext{ctx: ctx, r: r})
	return err
}

func cleanBundlePath(name string) (string, error) {
	if name == "" || path.IsAbs(name) {
		return "", fmt.Errorf("%w: unsafe path %q", ErrInvalidSnapshotBundle, name)
	}
	clean := path.Clean(name)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: unsafe path %q", ErrInvalidSnapshotBundle, name)
	}
	return clean, nil
}

func snapshotMetadataJSON(metadata any) json.RawMessage {
	data, err := json.Marshal(metadata)
	if err != nil || string(data) == "null" {
		return nil
	}
	return data
}

type readerWithContext struct {
	ctx context.Context
	r   io.Reader
}

func (r readerWithContext) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
	}
	return r.r.Read(p)
}
