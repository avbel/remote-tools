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
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
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

	// Normalise and resolve the request path inside the root.
	urlPath := path.Clean("/" + r.URL.Path)
	full := filepath.Join(h.root, filepath.FromSlash(urlPath))
	// Defence in depth: make sure we never leave the root.
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
		if r.URL.Query().Get("archive") == "tgz" {
			h.serveArchive(w, r, full, urlPath)
			return
		}
		// Ensure trailing slash so relative links resolve correctly.
		if !strings.HasSuffix(r.URL.Path, "/") {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusMovedPermanently)
			return
		}
		h.serveListing(w, r, full, urlPath)
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

	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf("attachment; filename=%q", filepath.Base(full)))
	}
	// http.ServeContent handles Range, If-Modified-Since, and Content-Type.
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

type listingEntry struct {
	Name     string
	Href     string
	IsDir    bool
	Size     int64
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

func (h *handler) serveListing(w http.ResponseWriter, r *http.Request, full, urlPath string) {
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
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].IsDir(), entries[j].IsDir()
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	data := listingData{Path: urlPath}
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
		href := (&url{Path: name}).escape()
		if e.IsDir() {
			href += "/"
		}
		data.Entries = append(data.Entries, listingEntry{
			Name:     name,
			Href:     href,
			IsDir:    e.IsDir(),
			Size:     info.Size(),
			SizeHR:   humanSize(info.Size()),
			Modified: info.ModTime().Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = listingTmpl.Execute(w, data)
}

func (h *handler) serveArchive(w http.ResponseWriter, r *http.Request, full, urlPath string) {
	base := filepath.Base(full)
	if base == "." || base == "/" || base == "" {
		base = "archive"
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", base+".tar.gz"))

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	_ = filepath.WalkDir(full, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries silently
		}
		// Skip anything that isn't a regular file or directory; in particular,
		// skip symlinks so we never escape the root.
		info, err := d.Info()
		if err != nil {
			return nil
		}
		mode := info.Mode()
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
			return err // writer is broken; stop
		}
		if !info.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return nil
			}
			_, _ = io.Copy(tw, f)
			f.Close()
		}
		return nil
	})
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

// url is a small helper for per-segment URL escaping in links. Using
// net/url.URL would also pull in query parsing on every render; this is
// simpler and avoids re-escaping slashes.
type url struct{ Path string }

func (u *url) escape() string {
	// Escape per-segment; callers pass a single segment name.
	return (&urlSegment{u.Path}).String()
}

type urlSegment struct{ s string }

func (u *urlSegment) String() string {
	var b strings.Builder
	for _, r := range u.s {
		switch {
		case r == '/' || r == '?' || r == '#':
			fmt.Fprintf(&b, "%%%02X", r)
		case r <= 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "%%%02X", r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
