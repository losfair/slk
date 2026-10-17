//go:build linux || darwin

package localapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func socketTestDir(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "slk-api-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func TestSocketLifecycleAndDiscovery(t *testing.T) {
	dir := socketTestDir(t)
	path := filepath.Join(dir, "api.sock")
	s, err := Start(path, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, name := range []string{"api.sock", "api.json", "api.lock"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("unexpected files: %v", entries)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "api.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d Discovery
	if json.Unmarshal(contents, &d) != nil || d.SocketPath != path || d.InstanceID == "" {
		t.Fatal(string(contents))
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	resp, err := client.Get("http://slk.local/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if other, err := Start(path, &fakeBackend{}); err == nil {
		_ = other.Close()
		t.Fatal("second instance succeeded")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"api.sock", "api.json"} {
		if _, err = os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s remains: %v", name, err)
		}
	}
	next, err := Start(path, &fakeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.Discovery().InstanceID == d.InstanceID {
		t.Fatal("instance ID reused")
	}
}
func TestUnsafePathsAndLiveSockets(t *testing.T) {
	t.Run("permissions", func(t *testing.T) {
		dir := socketTestDir(t)
		_ = os.Chmod(dir, 0o755)
		if s, err := Start(filepath.Join(dir, "api.sock"), &fakeBackend{}); err == nil {
			_ = s.Close()
			t.Fatal("accepted public directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		dir := socketTestDir(t)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Fatal(err)
		}
		if s, err := Start(filepath.Join(link, "api.sock"), &fakeBackend{}); err == nil {
			_ = s.Close()
			t.Fatal("accepted symlink")
		}
	})
	t.Run("regular file", func(t *testing.T) {
		dir := socketTestDir(t)
		path := filepath.Join(dir, "api.sock")
		_ = os.WriteFile(path, []byte("keep"), 0o600)
		if s, err := Start(path, &fakeBackend{}); err == nil {
			_ = s.Close()
			t.Fatal("replaced file")
		}
		data, _ := os.ReadFile(path)
		if string(data) != "keep" {
			t.Fatal("file changed")
		}
	})
	t.Run("live socket", func(t *testing.T) {
		dir := socketTestDir(t)
		path := filepath.Join(dir, "api.sock")
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if s, err := Start(path, &fakeBackend{}); err == nil {
			_ = s.Close()
			t.Fatal("replaced live socket")
		}
	})
	t.Run("stale socket", func(t *testing.T) {
		dir := socketTestDir(t)
		path := filepath.Join(dir, "api.sock")
		l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		l.SetUnlinkOnClose(false)
		_ = l.Close()
		s, err := Start(path, &fakeBackend{})
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	})
}
func TestCloseCancelsInFlightAndPreservesForeignArtifacts(t *testing.T) {
	dir := socketTestDir(t)
	path := filepath.Join(dir, "api.sock")
	started := make(chan struct{})
	s, err := Start(path, &fakeBackend{history: func(ctx context.Context, _, _ string, _ PageRequest) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	done := make(chan struct{})
	go func() {
		defer close(done)
		client := &http.Client{Transport: transport, Timeout: time.Second}
		resp, err := client.Get("http://slk.local/v1/workspaces/T1/conversations/C1/messages")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	manifest := filepath.Join(dir, "api.json")
	_ = os.Remove(manifest)
	_ = os.WriteFile(manifest, []byte("replacement"), 0o600)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	data, _ := os.ReadFile(manifest)
	if string(data) != "replacement" {
		t.Fatal("removed replacement artifact")
	}
}

func TestSocketRejectsReplaceableParentAndReservedNames(t *testing.T) {
	dir := socketTestDir(t)
	parent := filepath.Join(dir, "shared")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	if s, err := Start(filepath.Join(parent, "private", "api.sock"), &fakeBackend{}); err == nil {
		_ = s.Close()
		t.Fatal("accepted a replaceable parent directory")
	}
	for _, name := range []string{"api.json", "api.lock"} {
		if s, err := Start(filepath.Join(dir, name), &fakeBackend{}); err == nil {
			_ = s.Close()
			t.Fatalf("accepted reserved socket name %s", name)
		}
	}
}
