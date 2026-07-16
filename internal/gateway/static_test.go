package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkStaticTextAsset(b *testing.B) {
	dir := b.TempDir()
	asset := []byte(strings.Repeat("const thornhill = 'durable approval parking';\n", 4096))
	writeStaticFile(b, dir, "assets/app-Dj5CxPkn.js", asset)
	h := staticHandler(dir)

	for _, encoding := range []string{"identity", "gzip"} {
		b.Run(encoding, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodGet, "/assets/app-Dj5CxPkn.js", nil)
			if encoding == "gzip" {
				req.Header.Set("Accept-Encoding", "gzip")
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(asset)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					b.Fatalf("status = %d", w.Code)
				}
			}
		})
	}
}

func TestStaticHashedTextAssetUsesGzipAndImmutableCache(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	asset := []byte(strings.Repeat("const thornhill = 'durable approval parking';\n", 512))
	writeStaticFile(t, dir, "assets/app-Dj5CxPkn.js", asset)

	req := httptest.NewRequest(http.MethodGet, "/assets/app-Dj5CxPkn.js", nil)
	req.Header.Set("Accept-Encoding", "br, gzip")
	w := httptest.NewRecorder()
	staticHandler(dir).ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != immutableAssetCacheControl {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Fatalf("Content-Length must be omitted for gzip response, got %q", got)
	}

	zr, err := gzip.NewReader(w.Result().Body)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decompressed, asset) {
		t.Fatal("gzip body changed the asset")
	}
}

func TestStaticAssetIdentityForRangeAndGzipDecline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	asset := []byte("0123456789")
	writeStaticFile(t, dir, "assets/app-Dj5CxPkn.js", asset)
	h := staticHandler(dir)

	t.Run("range", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app-Dj5CxPkn.js", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		req.Header.Set("Range", "bytes=2-5")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusPartialContent {
			t.Fatalf("status = %d", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("range Content-Encoding = %q", got)
		}
		if got := w.Body.String(); got != "2345" {
			t.Fatalf("range body = %q", got)
		}
		if got := w.Header().Get("Cache-Control"); got != immutableAssetCacheControl {
			t.Fatalf("range Cache-Control = %q", got)
		}
	})

	t.Run("gzip quality zero", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/assets/app-Dj5CxPkn.js", nil)
		req.Header.Set("Accept-Encoding", "br, gzip;q=0")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("declined Content-Encoding = %q", got)
		}
		if !bytes.Equal(w.Body.Bytes(), asset) {
			t.Fatal("identity asset body changed")
		}
	})
}

func TestStaticMissingAssetIsNotCachedOrCompressed(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/assets/missing-Dj5CxPkn.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	staticHandler(t.TempDir()).ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("missing Cache-Control = %q", got)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("missing Content-Encoding = %q", got)
	}
}

func TestStaticDocumentRevalidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeStaticFile(t, dir, "index.html", []byte("<!doctype html><title>Thornhill</title>"))
	w := httptest.NewRecorder()
	staticHandler(dir).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("document Cache-Control = %q", got)
	}
}

func writeStaticFile(t testing.TB, dir, name string, body []byte) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
}
