package encryptedfs

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A real child process owns the file lock and never closes the encrypted file.
// Process.Kill is portable across the three CI OSes. This verifies process-death
// recovery, not storage-controller behavior during a power failure.
func TestFileRecoveryAfterProcessKill(t *testing.T) {
	if os.Getenv("TERMCP_TEST_ENCRYPTEDFS_CHILD") == "1" {
		runCrashWriter(t)
		return
	}
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.enc")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFileRecoveryAfterProcessKill$")
	cmd.Env = append(os.Environ(),
		"TERMCP_TEST_ENCRYPTEDFS_CHILD=1",
		"TERMCP_TEST_ENCRYPTEDFS_PATH="+path,
		"TERMCP_TEST_ENCRYPTEDFS_KEY="+base64.StdEncoding.EncodeToString(key[:]),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
	})
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		ready <- scanner.Scan() && scanner.Text() == "synced"
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("child exited before synchronizing its records")
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the writer child")
	}
	if writer, err := OpenAppend(path, key, testIdentity); !errors.Is(err, ErrBusy) {
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatalf("child did not hold exclusive writer ownership: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	info, err := Inspect(path, key, testIdentity)
	if !errors.Is(err, ErrIncompleteTail) || info.LogicalSize != int64(len("synced output\r\n")) {
		t.Fatalf("killed writer prefix=%+v err=%v", info, err)
	}
	info, err = RecoverTail(path, key, testIdentity)
	if err != nil || info.BackupPath == "" {
		t.Fatalf("recovery did not preserve the incomplete ciphertext: %v", err)
	}
	f, err := OpenAppend(path, key, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if offset, err := f.Append([]byte("resumed")); err != nil || offset != info.LogicalSize {
		t.Fatalf("append after process death offset=%d err=%v", offset, err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "synced output\r\nresumed" {
		t.Fatal("process death lost or changed synchronized history")
	}
}

func runCrashWriter(t *testing.T) {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(os.Getenv("TERMCP_TEST_ENCRYPTEDFS_KEY"))
	if err != nil || len(decoded) != len(Key{}) {
		t.Fatal("invalid child test key")
	}
	var key Key
	copy(key[:], decoded)
	clear(decoded)
	f, err := Create(os.Getenv("TERMCP_TEST_ENCRYPTEDFS_PATH"), key, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Append([]byte("synced output\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	f.writer = partialRecordWriter{f.f}
	if _, err := f.Append([]byte("incomplete record")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("partial child write = %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(os.Stdout, "synced\n"); err != nil {
		t.Fatal(err)
	}
	// Keep the process and file alive until the parent kills it. Reading stdin
	// avoids a timed sleep or a goroutine-leak-prone blocked channel.
	var wait [1]byte
	_, _ = os.Stdin.Read(wait[:])
	t.Fatal("writer child must be terminated before closing its file")
}
