//go:build unix

package catalog

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadRejectsFIFO(t *testing.T) {
	for _, name := range []string{"catalog.json", ".indexignore", "linked.json"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			configs := filepath.Join(root, "configs")
			if err := os.Mkdir(configs, 0o700); err != nil {
				t.Fatal(err)
			}
			fifo := filepath.Join(configs, name)
			if name == "linked.json" {
				fifo = filepath.Join(root, "fifo")
			}
			if err := unix.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			if name == "linked.json" {
				if err := os.Symlink("../fifo", filepath.Join(configs, name)); err != nil {
					t.Fatal(err)
				}
			}
			for _, method := range []string{"ReadFS", "ReadFS without StatFS", "readExtracted"} {
				t.Run(method, func(t *testing.T) {
					done := make(chan error, 1)
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
					defer cancel()
					go func() {
						var snapshot *Snapshot
						var err error
						if method == "readExtracted" {
							snapshot, err = (Reader{}).readExtracted(ctx, Source{}, root, "/configs")
						} else {
							configsFS := os.DirFS(configs)
							if method == "ReadFS without StatFS" {
								configsFS = struct{ fs.FS }{configsFS}
							}
							snapshot, err = (Reader{}).ReadFS(ctx, Source{}, configsFS)
						}
						if snapshot != nil {
							done <- nil
							return
						}
						done <- err
					}()
					select {
					case err := <-done:
						want := "catalog file is not regular"
						if method == "ReadFS without StatFS" {
							want = "catalog filesystem must implement fs.StatFS"
						}
						if err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("want special-file rejection, got %v", err)
						}
					case <-time.After(time.Second):
						// Release a regressed blocking open without ever blocking the cleanup itself.
						fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0)
						if err == nil {
							_ = unix.Close(fd)
						}
						select {
						case err := <-done:
							_ = err // The timeout path only waits for the goroutine to finish.
						case <-time.After(time.Second):
						}
						t.Fatal("catalog FIFO read blocked without a writer")
					}
				})
			}
		})
	}
}
