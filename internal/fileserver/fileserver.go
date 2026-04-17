// Package fileserver serves a local directory over HTTP in read-only mode,
// with a human-friendly listing, forced-download links, range-aware file
// responses, and on-the-fly gzipped tar archives of a directory.
package fileserver

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	queryDownload = "download"
	queryArchive  = "archive"
	archiveTGZ    = "tgz"
)

// New returns an http.Handler exposing root read-only.
//
// Supported query params:
//
//	?download=1   -> force Content-Disposition: attachment
//	?archive=tgz  -> stream the requested directory as a gzip tarball
func New(root string) (http.Handler, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("fileserver: resolve %q: %w", root, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fileserver: stat %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fileserver: %q is not a directory", abs)
	}
	return &handler{root: abs}, nil
}

type handler struct {
	root string
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	urlPath := path.Clean("/" + r.URL.Path)
	full := filepath.Join(h.root, filepath.FromSlash(urlPath))
	// Defence in depth: http.Dir also blocks traversal, but belt-and-braces.
	rel, err := filepath.Rel(h.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	info, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "stat error", http.StatusInternalServerError)
		return
	}

	if info.IsDir() {
		if r.URL.Query().Get(queryArchive) == archiveTGZ {
			h.serveArchive(w, full)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		h.serveListing(w, full, urlPath)
		return
	}

	h.serveFile(w, r, full, info)
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, full string, info os.FileInfo) {
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "open error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	if r.URL.Query().Get(queryDownload) == "1" {
		w.Header().Set("Content-Disposition", contentDisposition(filepath.Base(full)))
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

type listingEntry struct {
	Name     string
	Href     string
	IsDir    bool
	SizeHR   string
	Modified string
}

type listingData struct {
	Path    string
	Parent  string
	Entries []listingEntry
}

var listingTmpl = template.Must(template.New("listing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Index of {{.Path}}</title>
<style>
  body { font: 14px/1.4 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
         margin: 2rem; color: #222; }
  h1 { font-size: 1.1rem; margin-bottom: 1rem; }
  table { border-collapse: collapse; width: 100%; max-width: 900px; }
  th, td { text-align: left; padding: 0.35rem 0.75rem; border-bottom: 1px solid #eee; }
  th { font-weight: 600; background: #fafafa; }
  td.size, th.size { text-align: right; font-variant-numeric: tabular-nums; }
  a { color: #0366d6; text-decoration: none; }
  a:hover { text-decoration: underline; }
  .dl { font-size: 0.85em; color: #666; }
  .tools { margin: 0 0 1rem 0; font-size: 0.9em; }
</style>
</head>
<body>
<h1>Index of {{.Path}}</h1>
<div class="tools">
  <a href="?archive=tgz">Download directory as .tar.gz</a>
</div>
<table>
  <thead>
    <tr><th>Name</th><th class="size">Size</th><th>Modified</th><th></th></tr>
  </thead>
  <tbody>
  {{if .Parent}}
    <tr><td><a href="{{.Parent}}">../</a></td><td class="size">&mdash;</td><td>&mdash;</td><td></td></tr>
  {{end}}
  {{range .Entries}}
    <tr>
      <td><a href="{{.Href}}">{{.Name}}{{if .IsDir}}/{{end}}</a></td>
      <td class="size">{{if .IsDir}}&mdash;{{else}}{{.SizeHR}}{{end}}</td>
      <td>{{.Modified}}</td>
      <td>{{if not .IsDir}}<a class="dl" href="{{.Href}}?download=1">download</a>{{end}}</td>
    </tr>
  {{end}}
  </tbody>
</table>
</body>
</html>
`))

func (h *handler) serveListing(w http.ResponseWriter, full, urlPath string) {
	f, err := os.Open(full)
	if err != nil {
		http.Error(w, "open error", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		http.Error(w, "readdir error", http.StatusInternalServerError)
		return
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name()), strings.ToLower(b.Name()))
	})

	data := listingData{
		Path:    urlPath,
		Entries: make([]listingEntry, 0, len(entries)),
	}
	if urlPath != "/" {
		data.Parent = path.Dir(strings.TrimRight(urlPath, "/")) + "/"
		if data.Parent == "//" {
			data.Parent = "/"
		}
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		href := url.PathEscape(name)
		if e.IsDir() {
			href += "/"
		}
		data.Entries = append(data.Entries, listingEntry{
			Name:     name,
			Href:     href,
			IsDir:    e.IsDir(),
			SizeHR:   humanSize(info.Size()),
			Modified: info.ModTime().Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := listingTmpl.Execute(w, data); err != nil {
		log.Printf("fileserver: listing template: %v", err)
	}
}

func (h *handler) serveArchive(w http.ResponseWriter, full string) {
	base := filepath.Base(full)
	if base == "." || base == "/" || base == "" {
		base = "archive"
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", contentDisposition(base+".tar.gz"))

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries silently
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode()
		// Skip symlinks so we never escape the root; skip anything that's
		// not a regular file or directory.
		if mode&fs.ModeSymlink != 0 {
			return nil
		}
		if !mode.IsRegular() && !info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(full, p)
		if err != nil {
			return nil
		}
		if rel == "." {
			rel = base
		} else {
			rel = base + "/" + filepath.ToSlash(rel)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.IsDir() {
			if err := copyFileContents(tw, p); err != nil {
				return err
			}
		}
		return nil
	})

	// On success, write the tar footer so the archive is complete.
	// On error, skip Close() intentionally: the client's `tar -xzf` will
	// then abort loudly on truncated input instead of silently extracting
	// a partial tree. We still close the gzip stream so the response body
	// is a valid gzip file (the outer transport shouldn't be blamed).
	if err == nil {
		_ = tw.Close()
	} else {
		log.Printf("fileserver: archive walk aborted: %v", err)
	}
	_ = gz.Close()
}

// copyFileContents copies the file at path into w. Unreadable files are
// skipped (nil error) so one permission-denied file doesn't abort the whole
// archive; write errors propagate so the caller can abort the tar.
func copyFileContents(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// contentDisposition formats an RFC 6266 attachment header. Falls back to
// RFC 5987 filename* encoding for names mime.FormatMediaType can't represent
// (e.g. non-ASCII, quotes).
func contentDisposition(name string) string {
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return `attachment; filename*=UTF-8''` + url.PathEscape(name)
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
