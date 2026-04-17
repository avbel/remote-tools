package fileserver

import (
	"context"
	"io/fs"
	"net/http"
	"os"

	"golang.org/x/net/webdav"
)

const (
	davMountPoint     = "/dav/"
	davPrefix         = "/dav"
	webdavAllowHeader = "OPTIONS, HEAD, GET, PROPFIND"
)

var webdavReadMethods = map[string]bool{
	http.MethodOptions: true,
	http.MethodHead:    true,
	http.MethodGet:     true,
	"PROPFIND":         true,
}

// newWebDAV returns a read-only WebDAV handler for root, mounted at
// davPrefix. Write methods return 405 so clients see it as read-only at
// the protocol level.
func newWebDAV(root string) http.Handler {
	h := &webdav.Handler{
		Prefix:     davPrefix,
		FileSystem: readOnlyDir(root),
		LockSystem: webdav.NewMemLS(),
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !webdavReadMethods[r.Method] {
			w.Header().Set("Allow", webdavAllowHeader)
			http.Error(w, "read-only WebDAV", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// readOnlyDir is defence in depth: the HTTP-method filter already blocks
// mutating methods, but a permissive method that still goes through
// OpenFile with write flags must not be able to mutate the backing FS.
type readOnlyDir string

func (d readOnlyDir) Mkdir(_ context.Context, _ string, _ os.FileMode) error {
	return fs.ErrPermission
}

func (d readOnlyDir) RemoveAll(_ context.Context, _ string) error {
	return fs.ErrPermission
}

func (d readOnlyDir) Rename(_ context.Context, _, _ string) error {
	return fs.ErrPermission
}

func (d readOnlyDir) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	const writeFlags = os.O_WRONLY | os.O_RDWR | os.O_CREATE | os.O_APPEND | os.O_TRUNC
	if flag&writeFlags != 0 {
		return nil, fs.ErrPermission
	}
	return webdav.Dir(d).OpenFile(ctx, name, flag, perm)
}

func (d readOnlyDir) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	return webdav.Dir(d).Stat(ctx, name)
}
