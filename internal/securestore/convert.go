package securestore

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/sessionlog"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func convert(ctx context.Context, root, stage string, keys *encryptedfs.KeyManager, sources []source, report func(Progress)) error {
	planned := make(map[string]bool, len(sources))
	for _, entry := range sources {
		planned[entry.Path] = true
	}
	shells := make(map[string]int64)
	var totalBytes int64
	for _, entry := range sources {
		parts := strings.Split(entry.Path, "/")
		if parts[0] != "sessions" {
			continue
		}
		if !fsID(parts[1]) || !planned["sessions/"+parts[1]+"/manifest.json"] {
			return ErrLegacyFormat
		}
		if len(parts) == 4 {
			if !fsID(parts[2]) || !planned["sessions/"+parts[1]+"/"+parts[2]+"/manifest.json"] {
				return ErrLegacyFormat
			}
			shell := parts[1] + "/" + parts[2]
			if _, exists := shells[shell]; !exists {
				shells[shell] = 0
			}
			if parts[3] == "log.bin" {
				shells[shell] = entry.Size
				totalBytes += entry.Size
			}
		}
	}
	totalUnits := len(sources) + len(shells)
	if err := encryptedfs.MkdirAllDurable(stage); err != nil {
		return err
	}
	// Configs are a small atomic container. Rebuild from the frozen legacy plan,
	// rather than allowing stale staging entries to survive an interrupted run.
	hostPath := filepath.Join(stage, "config", "hosts.enc")
	if err := os.Remove(hostPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "store.enc")); err == nil {
		data, err := os.ReadFile(filepath.Join(root, "config", "hosts.enc"))
		if err != nil {
			return err
		}
		if err := encryptedfs.MkdirAllDurable(filepath.Dir(hostPath)); err != nil {
			return err
		}
		if err := os.WriteFile(hostPath, data, 0600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hosts, err := sshconfig.NewEncryptedStore(stage, keys)
	if err != nil {
		return err
	}
	templates, err := extensions.NewEncryptedStore(stage, keys)
	if err != nil {
		return err
	}
	existingTemplates, err := extensions.NewEncryptedStore(root, keys)
	if err != nil {
		return err
	}
	store, err := storage.NewEncrypted(stage, keys)
	if err != nil {
		return err
	}
	defer store.Close()
	for i, entry := range sources {
		report(Progress{Stage: "convert", Completed: i, Total: totalUnits, BytesTotal: totalBytes})
		if err := ctx.Err(); err != nil {
			return err
		}
		parts := strings.Split(entry.Path, "/")
		if len(parts) == 3 && parts[0] == "extensions" && parts[2] == "config.toml" && !strings.HasPrefix(parts[1], ".") {
			data, err := readSmall(filepath.Join(root, filepath.FromSlash(entry.Path)))
			if err != nil {
				return err
			}
			existing, readErr := existingTemplates.ReadRaw(parts[1])
			if readErr == nil && !bytes.Equal(existing, data) {
				clear(existing)
				clear(data)
				return ErrLegacyChanged
			}
			clear(existing)
			if readErr != nil && !errors.Is(readErr, extensions.ErrNotFound) {
				clear(data)
				return readErr
			}
			err = templates.ImportRaw(parts[1], data)
			if err == nil {
				var got []byte
				got, err = templates.ReadRaw(parts[1])
				if err == nil && !bytes.Equal(data, got) {
					err = encryptedfs.ErrFormat
				}
				clear(got)
			}
			clear(data)
			if err != nil {
				return err
			}
		}
		if len(parts) == 3 && parts[0] == "ssh_configs" && parts[2] == "config.toml" {
			name := parts[1]
			if name == "internal" || strings.HasPrefix(name, ".") && name != ".internal" {
				continue // legacy List ignored these; exact bytes remain archived
			}
			if name == ".internal" {
				name = "internal"
			}
			data, err := readSmall(filepath.Join(root, filepath.FromSlash(entry.Path)))
			if err != nil {
				return err
			}
			err = hosts.Save(name, data)
			clear(data)
			if err != nil {
				return ErrLegacyFormat
			}
		}
		if parts[0] != "sessions" {
			continue
		}
		if parts[len(parts)-1] != "manifest.json" {
			continue
		}
		data, err := readSmall(filepath.Join(root, filepath.FromSlash(entry.Path)))
		if err != nil {
			return err
		}
		shellID := ""
		if len(parts) == 4 {
			shellID = parts[2]
		}
		err = store.ImportMetadata(parts[1], shellID, data)
		clear(data)
		if err != nil {
			return err
		}
	}
	if err := hosts.EnsureEncryptedDocument(); err != nil {
		return err
	}
	names := make([]string, 0, len(shells))
	for shell := range shells {
		names = append(names, shell)
	}
	sort.Strings(names)
	var completedBytes int64
	for i, shell := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		update := func(n int64) {
			report(Progress{Stage: "convert", Completed: len(sources) + i, Total: totalUnits, BytesCompleted: completedBytes + n, BytesTotal: totalBytes})
		}
		update(0)
		if err := convertShell(ctx, root, stage, keys, shell, update); err != nil {
			return err
		}
		completedBytes += shells[shell]
	}
	report(Progress{Stage: "convert", Completed: totalUnits, Total: totalUnits, BytesCompleted: completedBytes, BytesTotal: totalBytes})
	return store.Close()
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<20)+1))
	if len(data) > 64<<20 {
		clear(data)
		return nil, encryptedfs.ErrTooLarge
	}
	return data, err
}

func legacyMarks(path string, size int64) ([]api.LogMark, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var marks []api.LogMark
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		if len(scanner.Bytes()) == 0 {
			continue
		}
		var mark api.LogMark
		if json.Unmarshal(scanner.Bytes(), &mark) != nil || mark.Offset < 0 || mark.Offset > size || mark.Time < 0 || mark.Status == "" || len(marks) != 0 && mark.Offset < marks[len(marks)-1].Offset {
			return nil, ErrLegacyFormat
		}
		marks = append(marks, mark)
	}
	return marks, scanner.Err()
}

func legacyStreams(path string, size int64) ([]int64, error) {
	data, err := readSmall(path)
	if errors.Is(err, os.ErrNotExist) {
		if size == 0 {
			return nil, nil
		}
		return []int64{1}, nil // old histories without log.stdio mean stdout
	}
	if err != nil {
		return nil, err
	}
	defer clear(data)
	if len(data)%8 != 0 {
		return nil, ErrLegacyFormat
	}
	var records []int64
	lastOffset := int64(-1)
	for i := 0; i < len(data); i += 8 {
		v := int64(binary.LittleEndian.Uint64(data[i:]))
		offset, stream, valid := storage.DecodeStdioRecord(v)
		if !valid || offset <= lastOffset || offset < 0 || offset >= size {
			return nil, ErrLegacyFormat
		}
		if len(records) == 0 && offset != 0 {
			records = append(records, 1)
		}
		if len(records) == 0 || (records[len(records)-1] < 0) != (stream == storage.StdioErr) {
			records = append(records, v)
		}
		lastOffset = offset
	}
	if len(records) == 0 && size != 0 {
		records = []int64{1}
	}
	return records, nil
}

func convertShell(ctx context.Context, root, stage string, keys *encryptedfs.KeyManager, shell string, report func(int64)) error {
	parts := strings.Split(shell, "/")
	if len(parts) != 2 || !fsID(parts[0]) || !fsID(parts[1]) {
		return ErrLegacyFormat
	}
	dir := filepath.Join(root, "sessions", filepath.FromSlash(shell))
	binPath := filepath.Join(dir, "log.bin")
	size := int64(0)
	if info, err := os.Stat(binPath); err == nil {
		size = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	marks, err := legacyMarks(filepath.Join(dir, "log.jsonl"), size)
	if err != nil {
		return err
	}
	streams, err := legacyStreams(filepath.Join(dir, "log.stdio"), size)
	if err != nil {
		return err
	}
	identity := "sessions/" + shell + "/events"
	key, err := keys.Derive("events", identity)
	if err != nil {
		return err
	}
	target := filepath.Join(stage, "sessions", filepath.FromSlash(shell), "events.enc")
	if verifyProjection(ctx, target, key, identity, binPath, size, marks, streams) == nil {
		return nil // resume a fully converted, reverified shell
	}
	if err := encryptedfs.MkdirAllDurable(filepath.Dir(target)); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log, err := sessionlog.Create(target, key, identity)
	if err != nil {
		return err
	}
	defer log.Close()
	var body *os.File
	if size != 0 {
		body, err = os.Open(binPath)
		if err != nil {
			return err
		}
		defer body.Close()
	}
	boundaries := []int64{0, size}
	for _, mark := range marks {
		boundaries = append(boundaries, mark.Offset)
	}
	for _, record := range streams {
		offset, _, _ := storage.DecodeStdioRecord(record)
		boundaries = append(boundaries, offset)
	}
	sort.Slice(boundaries, func(i, j int) bool { return boundaries[i] < boundaries[j] })
	markAt, streamAt, timestamp := 0, 0, int64(0)
	recordMarks := func(offset int64) error {
		for markAt < len(marks) && marks[markAt].Offset == offset {
			if err := log.RecordMark(marks[markAt]); err != nil {
				return err
			}
			timestamp = marks[markAt].Time
			markAt++
		}
		return nil
	}
	buf := make([]byte, 256<<10)
	defer clear(buf)
	lastReport := time.Now()
	for i := 0; i+1 < len(boundaries); i++ {
		position, end := boundaries[i], boundaries[i+1]
		if position == end {
			continue
		}
		if err := recordMarks(position); err != nil {
			return err
		}
		for streamAt+1 < len(streams) {
			offset, _, _ := storage.DecodeStdioRecord(streams[streamAt+1])
			if offset > position {
				break
			}
			streamAt++
		}
		stream := sessionlog.Stdout
		if len(streams) != 0 && streams[streamAt] < 0 {
			stream = sessionlog.Stderr
		}
		for position < end {
			if err := ctx.Err(); err != nil {
				return err
			}
			n := int(min(int64(len(buf)), end-position))
			if read, err := body.ReadAt(buf[:n], position); err != nil || read != n {
				return ErrLegacyChanged
			}
			if _, err := log.RecordOutput(timestamp, buf[:n], stream); err != nil {
				return err
			}
			position += int64(n)
			if time.Since(lastReport) >= 250*time.Millisecond {
				report(position)
				lastReport = time.Now()
			}
		}
	}
	if err := recordMarks(size); err != nil {
		return err
	}
	indexKey, err := keys.Derive("index", identity)
	if err != nil {
		return err
	}
	if err := log.SaveIndex(filepath.Join(filepath.Dir(target), "events.idx.enc"), indexKey); err != nil {
		return err
	}
	if err := log.Close(); err != nil {
		return err
	}
	return verifyProjection(ctx, target, key, identity, binPath, size, marks, streams)
}

func verifyProjection(ctx context.Context, path string, key encryptedfs.Key, identity, legacy string, size int64, marks []api.LogMark, streams []int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log, err := sessionlog.Open(path, key, identity)
	if err != nil {
		return err
	}
	defer log.Close()
	gotSize, err := log.Size()
	if err != nil || gotSize != size {
		return ErrLegacyChanged
	}
	actual := sha256.New()
	if _, err := io.Copy(actual, io.NewSectionReader(log, 0, size)); err != nil {
		return err
	}
	if size != 0 {
		expected, n, err := fingerprint(legacy, -1)
		if err != nil || n != size || !equalDigest(expected, actual.Sum(nil)) {
			return ErrLegacyChanged
		}
	}
	gotMarks, err := log.ReadMarks()
	if err != nil || !reflect.DeepEqual(gotMarks, marks) {
		return ErrLegacyFormat
	}
	gotStreams, err := log.ReadStdioRecords()
	if err != nil || !reflect.DeepEqual(gotStreams, streams) {
		return ErrLegacyFormat
	}
	return nil
}

func fsID(id string) bool {
	if id == "" {
		return false
	}
	for _, b := range []byte(id) {
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '_') {
			return false
		}
	}
	return true
}
