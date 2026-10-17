package localapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Discovery is atomically published next to the socket after startup.
type Discovery struct {
	SocketPath   string   `json:"socket_path"`
	PID          int      `json:"pid"`
	InstanceID   string   `json:"instance_id"`
	APIVersions  []string `json:"api_versions"`
	Capabilities []string `json:"capabilities"`
}

type Server struct {
	http      *http.Server
	listener  net.Listener
	cancel    context.CancelFunc
	lock      *os.File
	files     map[string]os.FileInfo
	once      sync.Once
	err       error
	discovery Discovery
}

func (s *Server) Discovery() Discovery {
	d := s.discovery
	d.APIVersions = append([]string(nil), d.APIVersions...)
	d.Capabilities = append([]string(nil), d.Capabilities...)
	return d
}

// Start binds a private socket. An empty path selects the platform default.
func Start(socketPath string, backend Backend) (*Server, error) {
	path, lock, err := prepareSocket(socketPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cancel: cancel, lock: lock, files: make(map[string]os.FileInfo)}
	failed := true
	defer func() {
		if failed {
			_ = s.Close()
		}
	}()
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("local API listen: %w", err)
	}
	s.listener = listener
	if l, ok := listener.(*net.UnixListener); ok {
		l.SetUnlinkOnClose(false)
	}
	// Track ownership before chmod so cleanup also covers a chmod failure.
	s.files[path], err = os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	s.discovery = Discovery{SocketPath: path, PID: os.Getpid(), InstanceID: randomID(), APIVersions: []string{"v1"}, Capabilities: []string{"read", "write"}}
	manifest := filepath.Join(filepath.Dir(path), "api.json")
	if err = checkPrivateFile(manifest); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".api-*.json")
	if err != nil {
		return nil, err
	}
	temp := file.Name()
	defer os.Remove(temp)
	err = json.NewEncoder(file).Encode(s.discovery)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err = os.Rename(temp, manifest); err != nil {
		return nil, err
	}
	s.files[manifest], err = os.Lstat(manifest)
	if err != nil {
		return nil, err
	}
	s.http = &http.Server{
		Handler:           newHandler(ctx, backend, s.discovery.InstanceID),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("local API listener stopped: %v", err)
			cancel()
		}
	}()
	failed = false
	return s, nil
}

// Close cancels operations and removes only artifacts created by this instance.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.http != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			s.err = s.http.Shutdown(ctx)
			cancel()
			if s.err != nil {
				_ = s.http.Close()
			}
		} else if s.listener != nil {
			s.err = s.listener.Close()
		}
		for path, owned := range s.files {
			if owned == nil {
				continue
			}
			if filepath.Base(path) == "api.json" {
				data, err := os.ReadFile(path)
				var current Discovery
				if err != nil || json.Unmarshal(data, &current) != nil || current.InstanceID != s.discovery.InstanceID {
					continue
				}
			}
			if info, err := os.Lstat(path); err == nil && os.SameFile(info, owned) {
				_ = os.Remove(path)
			}
		}
		if s.lock != nil {
			_ = s.lock.Close()
		}
	})
	return s.err
}
