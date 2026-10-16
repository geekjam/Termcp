package securestore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/sessionlog"
)

type State string

const (
	Uninitialized  State = "UNINITIALIZED"
	Locked         State = "LOCKED"
	Migrating      State = "MIGRATING"
	Recovering     State = "RECOVERING"
	Ready          State = "READY"
	CleanupPending State = "CLEANUP_PENDING"
	Failed         State = "FAILED"
	Closed         State = "CLOSED"
)

var (
	ErrState       = errors.New("securestore: operation is unavailable in the current storage state")
	ErrRateLimited = errors.New("securestore: too many password attempts; retry later")
	ErrMissingKey  = errors.New("securestore: encrypted data exists but the key envelope is missing")
)

type Status struct {
	State              State    `json:"state"`
	PasswordConfigured bool     `json:"password_configured"`
	LegacyFound        bool     `json:"legacy_found"`
	Progress           Progress `json:"progress"`
	ErrorCode          string   `json:"error_code,omitempty"`
}

// Activate builds the business graph only after migration and recovery. The
// closer must join session writers before closing the store. No live pointers
// in a previously published handler are swapped during initialization.
type Activate func(*encryptedfs.KeyManager) (http.Handler, io.Closer, error)

type Options struct {
	PasswordCost     encryptedfs.Argon2Parameters
	SystemProvider   encryptedfs.KeyProvider
	InitializeSystem bool
}

type Controller struct {
	op       sync.Mutex
	mu       sync.RWMutex
	root     string
	lock     *encryptedfs.DirectoryLock
	keys     *encryptedfs.KeyManager
	status   Status
	handler  http.Handler
	closer   io.Closer
	activate Activate
	cost     encryptedfs.Argon2Parameters
	ctx      context.Context
	cancel   context.CancelFunc
	jobs     sync.WaitGroup
	closing  bool
	window   time.Time
	attempts int
}

func Open(root string, activate Activate) (*Controller, error) {
	options := Options{}
	switch os.Getenv("TERMCP_STORAGE_KEY_PROVIDER") {
	case "", "password":
	case "env":
		if os.Getenv(encryptedfs.EnvStorageKEK) == "" {
			return nil, encryptedfs.ErrProviderUnavailable
		}
	case "system":
		options.SystemProvider = encryptedfs.SystemProvider()
		options.InitializeSystem = true
	default:
		return nil, errors.New("securestore: unsupported TERMCP_STORAGE_KEY_PROVIDER")
	}
	return OpenWithOptions(root, activate, options)
}

func openController(root string, activate Activate, cost encryptedfs.Argon2Parameters) (*Controller, error) {
	return OpenWithOptions(root, activate, Options{PasswordCost: cost})
}

func OpenWithOptions(root string, activate Activate, options Options) (*Controller, error) {
	lock, err := encryptedfs.AcquireDirectoryLock(root)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Controller{root: abs, lock: lock, activate: activate, cost: options.PasswordCost, ctx: ctx, cancel: cancel, status: Status{State: Uninitialized}}
	_, err = os.Stat(c.keyPath())
	if err == nil {
		c.status.State = Locked
		providers, inspectErr := encryptedfs.KeyProviders(c.keyPath())
		if inspectErr != nil {
			_ = c.Close()
			return nil, inspectErr
		}
		for _, name := range providers {
			c.status.PasswordConfigured = c.status.PasswordConfigured || name == "password"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = c.Close()
		return nil, err
	} else {
		for _, name := range []string{"store.enc", "config/hosts.enc", ".migration"} {
			if _, err := os.Lstat(filepath.Join(abs, filepath.FromSlash(name))); err == nil {
				_ = c.Close()
				return nil, ErrMissingKey
			} else if !errors.Is(err, os.ErrNotExist) {
				_ = c.Close()
				return nil, err
			}
		}
		if err := filepath.WalkDir(filepath.Join(abs, "sessions"), func(path string, entry os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if !entry.IsDir() && filepath.Ext(path) == ".enc" {
				return ErrMissingKey
			}
			return nil
		}); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	if _, err := os.Stat(filepath.Join(abs, "store.enc")); errors.Is(err, os.ErrNotExist) {
		found, detectErr := detectLegacy(abs)
		if detectErr != nil {
			_ = c.Close()
			return nil, detectErr
		}
		c.status.LegacyFound = found
	}
	if c.status.LegacyFound {
		slog.Warn("检测到旧版持久化文件；完成密钥设置或解锁后将迁移为加密存储。迁移期间服务暂不可用，请确保旧进程已停止并已备份数据目录")
	}
	if value, set := os.LookupEnv(encryptedfs.EnvStorageKEK); set && value != "" {
		_ = value // never retain or log the environment secret
		provider := encryptedfs.EnvProvider{}
		var keys *encryptedfs.KeyManager
		if c.status.State == Uninitialized {
			keys, err = encryptedfs.NewKeyManager(provider)
			if err == nil {
				err = encryptedfs.MkdirAllDurable(filepath.Dir(c.keyPath()))
			}
			if err == nil {
				err = keys.Save(c.keyPath())
			}
		} else {
			keys, err = encryptedfs.LoadKeyManager(c.keyPath(), provider)
		}
		if err != nil {
			c.status.ErrorCode = "environment_unlock_failed"
			if _, statErr := os.Stat(c.keyPath()); statErr == nil {
				c.status.State = Locked
			}
		} else {
			c.begin(keys)
		}
	} else if provider := options.SystemProvider; provider != nil && (c.status.State == Locked || options.InitializeSystem) {
		var keys *encryptedfs.KeyManager
		if c.status.State == Uninitialized {
			keys, err = encryptedfs.NewKeyManager(provider)
			if err == nil {
				err = encryptedfs.MkdirAllDurable(filepath.Dir(c.keyPath()))
			}
			if err == nil {
				err = keys.Save(c.keyPath())
			}
		} else {
			keys, err = encryptedfs.LoadKeyManager(c.keyPath(), provider)
		}
		if err != nil {
			c.status.ErrorCode = "system_unlock_failed"
			if _, statErr := os.Stat(c.keyPath()); statErr == nil {
				c.status.State = Locked
			}
		} else {
			c.begin(keys)
		}
	}
	return c, nil
}

func (c *Controller) keyPath() string { return filepath.Join(c.root, "keys", "master.key") }

func (c *Controller) Status() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.status
}

func (c *Controller) Handler() http.Handler {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.handler
}

func (c *Controller) attempt() error {
	if c.closing {
		return os.ErrClosed
	}
	if time.Since(c.window) >= time.Minute {
		c.window, c.attempts = time.Now(), 0
	}
	if c.attempts >= 5 {
		return ErrRateLimited
	}
	c.attempts++
	return nil
}

func (c *Controller) Initialize(password, confirmation []byte) error {
	c.op.Lock()
	defer c.op.Unlock()
	if c.Status().State != Uninitialized {
		return ErrState
	}
	if err := c.attempt(); err != nil {
		return err
	}
	keys, err := encryptedfs.NewPasswordKeyManager(password, confirmation, c.cost)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(c.keyPath()); err == nil {
		return ErrState
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := encryptedfs.MkdirAllDurable(filepath.Dir(c.keyPath())); err != nil {
		return err
	}
	if err := keys.Save(c.keyPath()); err != nil {
		if _, statErr := os.Stat(c.keyPath()); statErr == nil {
			c.mu.Lock()
			c.status.State, c.status.ErrorCode = Locked, "key_save_uncertain"
			c.mu.Unlock()
		}
		return err
	}
	c.begin(keys)
	return nil
}

func (c *Controller) Unlock(password []byte) error {
	c.op.Lock()
	defer c.op.Unlock()
	state := c.Status().State
	if state != Locked && state != Failed {
		return ErrState
	}
	if err := c.attempt(); err != nil {
		return err
	}
	provider, err := encryptedfs.NewPasswordProvider(password, c.cost)
	if err != nil {
		return err
	}
	defer provider.Destroy()
	keys, err := encryptedfs.LoadKeyManager(c.keyPath(), provider)
	if err != nil {
		return err
	}
	c.begin(keys)
	return nil
}

func (c *Controller) ChangePassword(current, password, confirmation []byte) error {
	c.op.Lock()
	defer c.op.Unlock()
	state := c.Status().State
	if state != Ready && state != CleanupPending {
		return ErrState
	}
	if err := c.attempt(); err != nil {
		return err
	}
	err := c.keys.ChangePasswordAndSave(c.keyPath(), current, password, confirmation, c.cost)
	if err == nil || errors.Is(err, encryptedfs.ErrDurability) {
		c.mu.Lock()
		c.status.ErrorCode = ""
		if err != nil {
			c.status.ErrorCode = "key_save_uncertain"
		}
		c.mu.Unlock()
	}
	return err
}

func (c *Controller) begin(keys *encryptedfs.KeyManager) {
	c.mu.Lock()
	c.keys = keys
	c.status.State, c.status.ErrorCode = Recovering, ""
	c.status.PasswordConfigured = keys.HasProvider("password")
	c.mu.Unlock()
	c.jobs.Add(1)
	go func() {
		defer c.jobs.Done()
		pending, err := Prepare(c.ctx, c.root, keys, func(progress Progress) {
			c.mu.Lock()
			changed := c.status.Progress.Stage != progress.Stage
			c.status.State, c.status.Progress = Migrating, progress
			c.mu.Unlock()
			if changed {
				slog.Info("encrypted storage migration", "stage", progress.Stage, "resuming", progress.Resuming)
			}
		})
		if err == nil {
			c.mu.Lock()
			c.status.State = Recovering
			c.mu.Unlock()
			err = recoverHistory(c.ctx, c.root, keys)
		}
		var handler http.Handler
		var closer io.Closer
		if err == nil && c.activate != nil {
			handler, closer, err = c.activate(keys)
		}
		if err == nil {
			err = c.ctx.Err()
		}
		if err != nil {
			if closer != nil {
				_ = closer.Close()
			}
			c.mu.Lock()
			c.status.State, c.status.ErrorCode = Failed, "storage_prepare_failed"
			if errors.Is(err, ErrLegacyChanged) {
				c.status.ErrorCode = "legacy_source_changed"
			} else if errors.Is(err, ErrLegacyFormat) {
				c.status.ErrorCode = "legacy_data_requires_repair"
			}
			c.mu.Unlock()
			slog.Error("加密存储迁移或恢复失败；提交前的旧文件会保留，修复原因后重新解锁可重试")
			return
		}
		c.mu.Lock()
		c.handler, c.closer = handler, closer
		c.status.State, c.status.Progress = Ready, Progress{}
		if pending {
			c.status.State = CleanupPending
		}
		c.mu.Unlock()
		if pending {
			slog.Warn("CLEANUP_PENDING：加密存储已可用，但仍有旧明文未完成清理")
		} else {
			slog.Info("加密存储已就绪；历史迁移、验证及明文清理完成")
		}
	}()
}

func (c *Controller) Close() error {
	c.op.Lock()
	c.closing = true
	c.cancel()
	c.op.Unlock()
	c.jobs.Wait()
	c.op.Lock()
	defer c.op.Unlock()
	c.mu.Lock()
	closer := c.closer
	c.closer, c.handler = nil, nil
	c.status.State = Closed
	c.mu.Unlock()
	var err error
	if closer != nil {
		err = closer.Close()
	}
	return errors.Join(err, c.lock.Close())
}

func recoverHistory(ctx context.Context, root string, keys *encryptedfs.KeyManager) error {
	return filepath.WalkDir(filepath.Join(root, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "events.enc" {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join(root, "sessions"), filepath.Dir(path))
		if err != nil || !filepath.IsLocal(rel) {
			return encryptedfs.ErrFormat
		}
		identity := "sessions/" + filepath.ToSlash(rel) + "/events"
		key, err := keys.Derive("events", identity)
		if err != nil {
			return err
		}
		_, err = encryptedfs.Inspect(path, key, identity)
		if errors.Is(err, encryptedfs.ErrIncompleteTail) {
			// Explicit application policy: preserve the entire ciphertext before
			// repairing only incomplete final framing; authenticated corruption
			// is never repaired. This does not prove the cause was a crash.
			slog.Warn("历史末尾存在未完成记录；将先保存原始密文副本再恢复有效前缀")
			_, err = encryptedfs.RecoverTail(path, key, identity)
		}
		if err != nil {
			return err
		}
		log, err := sessionlog.Open(path, key, identity)
		if err != nil {
			return err
		}
		indexKey, err := keys.Derive("index", identity)
		if err == nil {
			indexPath := filepath.Join(filepath.Dir(path), "events.idx.enc")
			if log.ValidateIndex(indexPath, indexKey) != nil {
				err = log.SaveIndex(indexPath, indexKey)
			}
		}
		return errors.Join(err, log.Close())
	})
}
