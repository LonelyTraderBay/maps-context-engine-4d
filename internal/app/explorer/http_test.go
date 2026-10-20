package explorer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGraphRequestRejectsLimitsOutsideFrozenSchema(t *testing.T) {
	service := &Service{}
	for _, raw := range []string{
		`{"root":{"kind":"workspace"},"depth":-1}`,
		`{"root":{"kind":"workspace"},"depth":9}`,
		`{"root":{"kind":"workspace"},"node_limit":501}`,
		`{"root":{"kind":"workspace"},"edge_limit":1501}`,
		`{"root":{"kind":"workspace"},"kinds":["symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol","symbol"]}`,
	} {
		response, err := service.graphResponse(context.Background(), []byte(raw))
		if err != nil {
			t.Fatalf("graphResponse(%s): %v", raw, err)
		}
		if !strings.Contains(string(response), `"code":"INVALID_ARGUMENT"`) {
			t.Fatalf("graphResponse(%s)=%s; want INVALID_ARGUMENT", raw, response)
		}
	}
}

func TestParseLoopbackAddress(t *testing.T) {
	for _, value := range []string{"", "127.0.0.1:0", "127.0.0.1:1", "127.0.0.1:65535"} {
		t.Run("accept/"+value, func(t *testing.T) {
			if _, err := parseLoopbackAddress(value); err != nil {
				t.Fatalf("parseLoopbackAddress(%q): %v", value, err)
			}
		})
	}
	for _, value := range []string{"localhost:0", "0.0.0.0:8080", "127.0.0.2:8080", "127.0.0.1:65536", "127.0.0.1:080", "127.0.0.1:http", "[::1]:8080", "127.0.0.1"} {
		t.Run("reject/"+value, func(t *testing.T) {
			if _, err := parseLoopbackAddress(value); err == nil {
				t.Fatalf("parseLoopbackAddress(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestStaticHandlerServesOnlyRegularAssets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<!doctype html><title>Explorer fixture</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "app.js"), []byte("window.fixture = true"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := newStaticHandler(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantBody   string
	}{
		{name: "index", method: http.MethodGet, path: "/", wantStatus: http.StatusOK, wantBody: "Explorer fixture"},
		{name: "asset", method: http.MethodGet, path: "/assets/app.js", wantStatus: http.StatusOK, wantBody: "window.fixture"},
		{name: "head", method: http.MethodHead, path: "/", wantStatus: http.StatusOK},
		{name: "hidden", method: http.MethodGet, path: "/.env", wantStatus: http.StatusNotFound},
		{name: "missing", method: http.MethodGet, path: "/missing.js", wantStatus: http.StatusNotFound},
		{name: "method", method: http.MethodPost, path: "/", wantStatus: http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status=%d body=%q; want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.wantBody != "" && !strings.Contains(response.Body.String(), test.wantBody) {
				t.Fatalf("body=%q; want %q", response.Body.String(), test.wantBody)
			}
			if test.name == "method" && response.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("Allow=%q; want GET, HEAD", response.Header().Get("Allow"))
			}
			if test.wantStatus == http.StatusOK && response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("Cache-Control=%q; want no-store", response.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestVerifyAssetsTreeRejectsSymbolicLinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.js")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "outside.js")); err != nil {
		t.Skipf("symbolic links are unavailable to this test principal: %v", err)
	}
	if err := verifyAssetsTree(root); err == nil {
		t.Fatal("assets tree containing a symbolic link unexpectedly passed")
	}
}

func TestFindExplorerAssetsIgnoresWorkingDirectory(t *testing.T) {
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	}()

	decoy := t.TempDir()
	decoyDist := filepath.Join(decoy, "web", "explorer", "dist")
	if err := os.MkdirAll(decoyDist, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyDist, "index.html"), []byte("untrusted decoy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(decoy); err != nil {
		t.Fatal(err)
	}

	assets, err := findExplorerAssets()
	if err != nil {
		t.Fatal(err)
	}
	if insidePath(decoyDist, assets) {
		t.Fatalf("Explorer assets resolved from target working directory: %q", assets)
	}
}

func TestInsidePathRejectsSiblingPrefix(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "explorer")
	inside := filepath.Join(root, "assets", "app.js")
	sibling := filepath.Join(parent, "explorer-private", "app.js")
	if !insidePath(root, inside) {
		t.Fatal("in-root asset was rejected")
	}
	if insidePath(root, sibling) {
		t.Fatal("sibling path with a matching prefix was accepted")
	}
}
