// Package securestore coordinates unlocked storage, migration and startup.
package securestore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

var (
	ErrLegacyChanged = errors.New("securestore: legacy source changed; stop the old process before retrying")
	ErrLegacyFormat  = errors.New("securestore: legacy data requires repair; original files are retained")
)

type Progress struct {
	Stage          string `json:"stage,omitempty"`
	Completed      int    `json:"completed,omitempty"`
	Total          int    `json:"total,omitempty"`
	BytesCompleted int64  `json:"bytes_completed,omitempty"`
	BytesTotal     int64  `json:"bytes_total,omitempty"`
	Resuming       bool   `json:"resuming,omitempty"`
}

type source struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	Digest     []byte `json:"digest"`
	Diagnostic bool   `json:"diagnostic,omitempty"`
	Cleaned    bool   `json:"cleaned,omitempty"`
}

type migration struct {
	Version int      `json:"version"`
	ID      string   `json:"id"`
	Sources []source `json:"sources,omitempty"`
	Clean   bool     `json:"clean"`
}

func managedLegacy(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) == 3 && parts[0] == "ssh_configs" {
		return parts[2] == "config.toml" || strings.HasPrefix(parts[2], ".tmp-cfg-")
	}
	if len(parts) == 3 && parts[0] == "extensions" {
		return parts[2] == "config.toml" || strings.HasPrefix(parts[2], ".tmp-ext-")
	}
	if parts[0] == "sessions" && (len(parts) == 3 || len(parts) == 4) {
		name := parts[len(parts)-1]
		if strings.HasSuffix(name, ".enc") || strings.HasPrefix(name, ".tmp-enc-") || strings.HasPrefix(name, ".tmp-migrate-") {
			return false
		}
		return name == "manifest.json" || len(parts) == 4 && (name == "log.bin" || name == "log.jsonl" || name == "log.lines" || name == "log.stdio") || strings.HasPrefix(name, ".tmp-")
	}
	return path == "termcp.log"
}

func detectLegacy(root string) (bool, error) {
	found := false
	for _, tree := range []string{"sessions", "ssh_configs", "extensions"} {
		base := filepath.Join(root, tree)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == base {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				if managedLegacy(filepath.ToSlash(rel)) {
					found = true
					return fs.SkipAll
				}
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	info, err := os.Stat(filepath.Join(root, "termcp.log"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Size() != 0, nil
}

func fingerprint(path string, limit int64) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha256.New()
	var reader io.Reader = f
	if limit >= 0 {
		reader = io.LimitReader(f, limit)
	}
	n, err := io.Copy(h, reader)
	return h.Sum(nil), n, err
}

func inventory(ctx context.Context, root string) ([]source, error) {
	var out []source
	for _, tree := range []string{"ssh_configs", "sessions", "extensions"} {
		base := filepath.Join(root, tree)
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) && path == base {
				return nil
			}
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return ErrLegacyFormat
			}
			if entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || !managedLegacy(filepath.ToSlash(rel)) {
				return err
			}
			if !entry.Type().IsRegular() {
				return ErrLegacyFormat
			}
			digest, size, err := fingerprint(path, -1)
			if err != nil {
				return err
			}
			out = append(out, source{Path: filepath.ToSlash(rel), Size: size, Digest: digest})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if info, err := os.Lstat(filepath.Join(root, "termcp.log")); err == nil && info.Size() != 0 {
		if !info.Mode().IsRegular() {
			return nil, ErrLegacyFormat
		}
		digest, size, err := fingerprint(filepath.Join(root, "termcp.log"), info.Size())
		if err != nil {
			return nil, err
		}
		out = append(out, source{Path: "termcp.log", Size: size, Digest: digest, Diagnostic: true})
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return out, nil
}

func readDocument(root, name string, keys *encryptedfs.KeyManager, out *migration) error {
	key, err := keys.Derive("control", name)
	if err != nil {
		return err
	}
	data, err := encryptedfs.ReadFile(filepath.Join(root, filepath.FromSlash(name)), key, name)
	if err != nil {
		return err
	}
	defer clear(data)
	if json.Unmarshal(data, out) != nil || out.Version != 1 || len(out.ID) != 32 {
		return encryptedfs.ErrFormat
	}
	if _, err := hex.DecodeString(out.ID); err != nil {
		return encryptedfs.ErrFormat
	}
	for _, entry := range out.Sources {
		if !fs.ValidPath(entry.Path) || strings.ContainsAny(entry.Path, "\\:") || !managedLegacy(entry.Path) || entry.Size < 0 || len(entry.Digest) != sha256.Size || entry.Diagnostic != (entry.Path == "termcp.log") {
			return encryptedfs.ErrFormat
		}
	}
	return nil
}

func writeDocument(root, name string, keys *encryptedfs.KeyManager, value migration) error {
	key, err := keys.Derive("control", name)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	defer clear(data)
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := encryptedfs.MkdirAllDurable(filepath.Dir(path)); err != nil {
		return err
	}
	return encryptedfs.WriteFileAtomic(path, key, name, data)
}

// Prepare performs a stop-write migration. The encrypted control record is the
// only commit boundary. Staging artifacts never select the active store format.
// Ciphertext backups belong to their session/shell, or diagnostics for other
// sources. Successfully converted config copies are retired after cleanup.
func Prepare(ctx context.Context, root string, keys *encryptedfs.KeyManager, report func(Progress)) (bool, error) {
	if report == nil {
		report = func(Progress) {}
	}
	var plan migration
	err := readDocument(root, "store.enc", keys, &plan)
	if err == nil {
		if plan.Clean {
			// A store encrypted before extension support may still have the
			// templates introduced by dev. Migrate those additively, preserving
			// existing encrypted hosts and sessions.
			found := false
			base := filepath.Join(root, "extensions")
			walkErr := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) && path == base {
					return nil
				}
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				if !entry.IsDir() && managedLegacy(filepath.ToSlash(rel)) {
					found = true
					return fs.SkipAll
				}
				return nil
			})
			if walkErr != nil || !found {
				return false, walkErr
			}
			err = os.ErrNotExist
		} else {
			report(Progress{Stage: "cleanup", Total: len(plan.Sources), Resuming: true})
			return cleanup(ctx, root, keys, &plan)
		}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	journal := ".migration/active/state.enc"
	err = readDocument(root, journal, keys, &plan)
	resuming := err == nil
	userReport := report
	report = func(p Progress) { p.Resuming = resuming; userReport(p) }
	if errors.Is(err, os.ErrNotExist) {
		report(Progress{Stage: "inventory"})
		sources, err := inventory(ctx, root)
		if err != nil {
			return false, err
		}
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return false, err
		}
		plan = migration{Version: 1, ID: hex.EncodeToString(id[:]), Sources: sources}
		if err := writeDocument(root, journal, keys, plan); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	stage := filepath.Join(root, ".migration", plan.ID, "staging")
	var totalBytes, completedBytes int64
	for _, entry := range plan.Sources {
		totalBytes += entry.Size
	}
	for i, entry := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		update := func(n int64) {
			report(Progress{Stage: "archive", Completed: i, Total: len(plan.Sources), BytesCompleted: completedBytes + n, BytesTotal: totalBytes})
		}
		update(0)
		if err := archive(ctx, root, stage, plan.ID, keys, entry, update); err != nil {
			return false, err
		}
		completedBytes += entry.Size
	}
	report(Progress{Stage: "convert"})
	if err := convert(ctx, root, stage, keys, plan.Sources, report); err != nil {
		return false, err
	}
	report(Progress{Stage: "publish"})
	if err := publishTree(ctx, stage, root); err != nil {
		return false, err
	}
	for _, entry := range plan.Sources {
		if err := verifySource(root, entry); err != nil {
			return false, err
		}
	}
	current, err := inventory(ctx, root)
	if err != nil {
		return false, err
	}
	planned := make(map[string]bool, len(plan.Sources))
	for _, entry := range plan.Sources {
		planned[entry.Path] = true
	}
	for _, entry := range current {
		if !entry.Diagnostic && !planned[entry.Path] {
			return false, ErrLegacyChanged
		}
	}
	if err := writeDocument(root, "store.enc", keys, plan); err != nil {
		return false, err
	}
	report(Progress{Stage: "cleanup", Total: len(plan.Sources)})
	return cleanup(ctx, root, keys, &plan)
}

func verifySource(root string, entry source) error {
	path := filepath.Join(root, filepath.FromSlash(entry.Path))
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !entry.Diagnostic && info.Size() != entry.Size || entry.Diagnostic && info.Size() < entry.Size {
		return ErrLegacyChanged
	}
	digest, size, err := fingerprint(path, entry.Size)
	if err != nil {
		return err
	}
	if size != entry.Size || !equalDigest(digest, entry.Digest) {
		return ErrLegacyChanged
	}
	return nil
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var mismatch byte
	for i := range a {
		mismatch |= a[i] ^ b[i]
	}
	return mismatch == 0
}

func archive(ctx context.Context, root, stage, id string, keys *encryptedfs.KeyManager, entry source, report func(int64)) error {
	identity := "legacy/" + id + "/" + entry.Path
	key, err := keys.Derive("legacy", identity)
	if err != nil {
		return err
	}
	path := filepath.Join(stage, filepath.FromSlash(archivePath(id, entry.Path)))
	check := func() bool {
		f, err := encryptedfs.Open(path, key, identity)
		if err != nil {
			return false
		}
		defer f.Close()
		h := sha256.New()
		n, err := io.Copy(h, f)
		return err == nil && n == entry.Size && equalDigest(h.Sum(nil), entry.Digest)
	}
	if check() {
		return verifySource(root, entry)
	}
	if err := verifySource(root, entry); err != nil {
		return err
	}
	if err := encryptedfs.MkdirAllDurable(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	src, err := os.Open(filepath.Join(root, filepath.FromSlash(entry.Path)))
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := encryptedfs.Create(path, key, identity)
	if err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	defer clear(buf)
	reader := io.LimitReader(src, entry.Size)
	var copied int64
	lastReport := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, dst.Close())
		}
		n, readErr := reader.Read(buf)
		if n != 0 {
			if _, err := dst.Append(buf[:n]); err != nil {
				return errors.Join(err, dst.Close())
			}
			copied += int64(n)
			if time.Since(lastReport) >= 250*time.Millisecond {
				report(copied)
				lastReport = time.Now()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return errors.Join(readErr, dst.Close())
			}
			break
		}
	}
	if err := dst.Close(); err != nil {
		return err
	}
	if !check() {
		return ErrLegacyChanged
	}
	report(entry.Size)
	return nil
}

func archivePath(id, name string) string {
	parts := strings.Split(name, "/")
	if parts[0] == "sessions" && (len(parts) == 3 || len(parts) == 4) {
		// Keep backups inside their session/shell ownership boundary, so an
		// explicit history deletion also deletes its migration copies.
		return strings.Join(parts[:len(parts)-1], "/") + "/.legacy-" + id + "/" + parts[len(parts)-1] + ".enc"
	}
	return "diagnostics/legacy-" + id + "/" + name + ".enc"
}

func publishTree(ctx context.Context, stage, root string) error {
	return filepath.WalkDir(stage, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(path, ".enc") {
			return nil
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil || !filepath.IsLocal(rel) {
			return encryptedfs.ErrFormat
		}
		target := filepath.Join(root, rel)
		if err := encryptedfs.MkdirAllDurable(filepath.Dir(target)); err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-migrate-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		_, copyErr := io.Copy(tmp, src)
		if err := errors.Join(copyErr, tmp.Sync(), tmp.Close()); err != nil {
			return err
		}
		return encryptedfs.PublishCiphertext(tmp.Name(), target)
	})
}

func cleanup(ctx context.Context, root string, keys *encryptedfs.KeyManager, plan *migration) (bool, error) {
	pending := false
	for i := range plan.Sources {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		entry := &plan.Sources[i]
		if entry.Cleaned {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.Path))
		err := verifySource(root, *entry)
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		} else if err == nil {
			if entry.Diagnostic {
				var f *os.File
				f, err = os.OpenFile(path, os.O_RDWR, 0)
				if err == nil {
					err = errors.Join(f.Truncate(0), f.Sync(), f.Close())
				}
			} else {
				err = os.Remove(path)
			}
			if err == nil {
				err = encryptedfs.SyncParent(path)
			}
		}
		if err != nil {
			pending = true
			continue
		}
		entry.Cleaned = true
		if err := writeDocument(root, "store.enc", keys, *plan); err != nil {
			return true, err
		}
	}
	if pending {
		return true, nil
	}
	for _, entry := range plan.Sources {
		parts := strings.Split(entry.Path, "/")
		if len(parts) == 3 && parts[0] == "ssh_configs" {
			// An empty legacy profile directory still exposes its host label.
			// Remove only empty directories; unrelated user files are preserved.
			if err := removeEmptyAncestors(filepath.Join(root, "ssh_configs"), filepath.Dir(filepath.Join(root, filepath.FromSlash(entry.Path)))); err != nil {
				return true, err
			}
		}
		profile := len(parts) == 3 && parts[0] == "ssh_configs" && parts[2] == "config.toml" && parts[1] != "internal" && (!strings.HasPrefix(parts[1], ".") || parts[1] == ".internal")
		template := len(parts) == 3 && parts[0] == "extensions" && parts[2] == "config.toml" && !strings.HasPrefix(parts[1], ".")
		if profile || template {
			// hosts.enc preserves the exact raw configuration. Retiring this
			// temporary backup avoids retaining deleted credentials separately.
			path := filepath.Join(root, filepath.FromSlash(archivePath(plan.ID, entry.Path)))
			err := os.Remove(path)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return true, nil
			}
			if err == nil {
				err = encryptedfs.SyncParent(path)
			}
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return true, err
			}
			if err := removeEmptyAncestors(filepath.Join(root, "diagnostics"), filepath.Dir(path)); err != nil {
				return true, err
			}
		}
	}
	// Retire staging before the final clean marker, so a failed removal is
	// retried on restart. Otherwise later history deletion could leave an old
	// ciphertext copy in a permanently abandoned staging tree.
	if err := os.RemoveAll(filepath.Join(root, ".migration")); err != nil {
		return true, err
	}
	if err := encryptedfs.SyncParent(filepath.Join(root, ".migration")); err != nil {
		return true, err
	}
	plan.Clean, plan.Sources = true, nil
	if err := writeDocument(root, "store.enc", keys, *plan); err != nil {
		return true, err
	}
	return false, nil
}

func removeEmptyAncestors(stop, dir string) error {
	for dir != stop {
		rel, err := filepath.Rel(stop, dir)
		if err != nil || !filepath.IsLocal(rel) {
			return encryptedfs.ErrFormat
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			dir = filepath.Dir(dir)
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) != 0 {
			return nil
		}
		if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Another process may have added an unmanaged file since ReadDir.
			if entries, readErr := os.ReadDir(dir); readErr == nil && len(entries) != 0 {
				return nil
			}
			return err
		}
		if err := encryptedfs.SyncParent(dir); err != nil {
			return err
		}
		dir = filepath.Dir(dir)
	}
	return nil
}
