package explorer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	transport "github.com/LonelyTraderBay/maps-context-engine-4d/internal/explorer"
)

var ErrAssetsUnavailable = errors.New("EXPLORER_ASSETS_UNAVAILABLE")

// Run serves the built local Explorer UI and its authenticated API on IPv4 loopback.
func Run(ctx context.Context, options Options, listen string, output io.Writer) error {
	if ctx == nil || output == nil {
		return errors.New("Explorer context and output are required")
	}
	address, err := parseLoopbackAddress(listen)
	if err != nil {
		return err
	}
	service, err := NewService(options)
	if err != nil {
		return err
	}
	assets, err := findExplorerAssets()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp4", address)
	if err != nil {
		return fmt.Errorf("bind Explorer loopback listener: %w", err)
	}
	api, err := transport.NewServer(listener, service)
	if err != nil {
		_ = listener.Close()
		return err
	}
	defer api.Close()
	code, ok := api.TakeBootstrapCode()
	if !ok {
		return errors.New("create Explorer bootstrap code: code unavailable")
	}
	static, err := newStaticHandler(assets)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/explorer/v1/", api)
	mux.Handle("/", static)
	host := api.Host()
	server := &http.Server{
		Handler:           localSecurityHeaders(host, mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	if _, err := fmt.Fprintf(output, "Explorer: http://%s/\nBootstrap code: %s\n", host, code); err != nil {
		return fmt.Errorf("write Explorer startup details: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return fmt.Errorf("shutdown Explorer HTTP server: %w", err)
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func parseLoopbackAddress(value string) (string, error) {
	if value == "" {
		value = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil || host != "127.0.0.1" || port == "" {
		return "", errors.New("Explorer --listen must be 127.0.0.1:<port>")
	}
	if port == "0" {
		return value, nil
	}
	parsed, err := net.LookupPort("tcp", port)
	if err != nil || parsed < 1 || parsed > 65535 || fmt.Sprint(parsed) != port {
		return "", errors.New("Explorer --listen port must be a decimal integer from 0 through 65535")
	}
	return value, nil
}

func findExplorerAssets() (string, error) {
	candidates := make([]string, 0, 4)
	if _, sourceFile, _, ok := runtime.Caller(0); ok {
		projectRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", ".."))
		candidates = append(candidates, filepath.Join(projectRoot, "web", "explorer", "dist"))
	}
	if executable, err := os.Executable(); err == nil {
		binaryDir := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(binaryDir, "web", "explorer", "dist"),
			filepath.Join(binaryDir, "..", "web", "explorer", "dist"),
		)
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate = filepath.Clean(candidate)
		if _, exists := seen[candidate]; exists {
			continue
		}
		seen[candidate] = struct{}{}
		if verifyAssetsTree(candidate) == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: local web/explorer/dist is missing or contains unsupported files; run the pinned npm build", ErrAssetsUnavailable)
}

func verifyAssetsTree(root string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrAssetsUnavailable
	}
	indexInfo, err := os.Lstat(filepath.Join(root, "index.html"))
	if err != nil || !indexInfo.Mode().IsRegular() || indexInfo.Mode()&os.ModeSymlink != 0 {
		return ErrAssetsUnavailable
	}
	return filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrAssetsUnavailable
		}
		info, err := entry.Info()
		if err != nil || entry.Type()&os.ModeSymlink != 0 || !(info.IsDir() || info.Mode().IsRegular()) {
			return ErrAssetsUnavailable
		}
		return nil
	})
}

func newStaticHandler(root string) (http.Handler, error) {
	if err := verifyAssetsTree(root); err != nil {
		return nil, err
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "/.") {
			http.NotFound(w, r)
			return
		}
		localPath := filepath.Join(root, filepath.FromSlash(name))
		if !insidePath(root, localPath) {
			http.NotFound(w, r)
			return
		}
		info, err := os.Lstat(localPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			http.NotFound(w, r)
			return
		}
		file, err := os.Open(localPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, path.Base(name), info.ModTime(), file)
	}), nil
}

func insidePath(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return false
	}
	return true
}

func localSecurityHeaders(host string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != host {
			http.Error(w, "request host is not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
