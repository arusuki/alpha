package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxArchive = 512 << 20
const maxBinary = 256 << 20

type asset struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	DownloadURL string `json:"browser_download_url"`
}
type release struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []asset   `json:"assets"`
}
type github struct {
	client            *http.Client
	base, repo, token string
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (g github) get(ctx context.Context, address, accept string, dst io.Writer, limit int64) error {
	u, err := url.Parse(address)
	if err != nil {
		return err
	}
	base, _ := url.Parse(g.base)
	// Asset API URLs must stay on the configured API origin; redirects to the
	// signed download host are handled by net/http without forwarding the token.
	if u.Scheme != base.Scheme || u.Host != base.Host || u.User != nil {
		return fmt.Errorf("untrusted GitHub API URL: %s", address)
	}
	return g.request(ctx, address, accept, dst, limit, true)
}
func (g github) request(ctx context.Context, address, accept string, dst io.Writer, limit int64, api bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "alpha-updater")
	if api && g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	res, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return g.responseError(req.URL.Path, res, api)
	}
	n, err := io.Copy(dst, io.LimitReader(res.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return fmt.Errorf("GitHub response exceeds %d bytes", limit)
	}
	return nil
}

// Report evidence from GitHub instead of calling every 403 a rate-limit error.
func (g github) responseError(path string, res *http.Response, api bool) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 8192)).Decode(&body)
	message := strings.Join(strings.Fields(body.Message), " ")
	if g.token != "" {
		message = strings.ReplaceAll(message, g.token, "[redacted]")
	}
	if len(message) > 1024 {
		message = message[:1024]
	}
	details := []string{}
	if message != "" {
		details = append(details, message)
	}
	limited := res.StatusCode == http.StatusTooManyRequests || ((res.StatusCode == http.StatusForbidden) && (res.Header.Get("X-RateLimit-Remaining") == "0" || res.Header.Get("Retry-After") != "" || strings.Contains(strings.ToLower(message), "rate limit")))
	if limited {
		details = append(details, "GitHub rate limit exceeded; do not retry until the limit resets")
	}
	for _, key := range []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Used", "X-RateLimit-Reset", "Retry-After"} {
		if n, err := strconv.ParseInt(res.Header.Get(key), 10, 64); err == nil {
			value := strconv.FormatInt(n, 10)
			if key == "X-RateLimit-Reset" {
				value += " (" + time.Unix(n, 0).UTC().Format(time.RFC3339) + ")"
			}
			details = append(details, key+"="+value)
		}
	}
	if api {
		if g.token == "" {
			details = append(details, "authentication=none; set GH_TOKEN or GITHUB_TOKEN in the service environment")
		} else {
			details = append(details, "authentication=token")
		}
	}
	if res.StatusCode == http.StatusForbidden && !limited {
		details = append(details, "access denied; check repository permissions or proxy policy; rate limiting is not confirmed")
	}
	return fmt.Errorf("GitHub %s: %s (%s)", path, res.Status, strings.Join(details, "; "))
}

// Public assets have a download URL that does not consume REST API requests.
// Keep authenticated downloads on the asset API for private repositories.
func (g github) downloadAsset(ctx context.Context, a asset, dst io.Writer, limit int64) error {
	if g.token == "" && a.DownloadURL != "" {
		u, err := url.Parse(a.DownloadURL)
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || !strings.HasPrefix(u.Path, "/"+g.repo+"/releases/download/") {
			return fmt.Errorf("untrusted GitHub asset download URL")
		}
		return g.request(ctx, a.DownloadURL, "application/octet-stream", dst, limit, false)
	}
	return g.get(ctx, a.URL, "application/octet-stream", dst, limit)
}
func (g github) latest(ctx context.Context, prerelease bool) (release, error) {
	endpoint := g.base + "/repos/" + g.repo + "/releases"
	if !prerelease {
		var b strings.Builder
		if err := g.get(ctx, endpoint+"/latest", "application/vnd.github+json", &b, 4<<20); err != nil {
			return release{}, err
		}
		var r release
		if err := json.Unmarshal([]byte(b.String()), &r); err != nil {
			return r, err
		}
		if r.Draft || r.Prerelease {
			return r, fmt.Errorf("latest release is not a published stable release")
		}
		return r, nil
	}
	var latest release
	for page := 1; page <= 100; page++ {
		var b strings.Builder
		if err := g.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), "application/vnd.github+json", &b, 16<<20); err != nil {
			return latest, err
		}
		var releases []release
		if err := json.Unmarshal([]byte(b.String()), &releases); err != nil {
			return latest, err
		}
		for _, r := range releases {
			if !r.Draft && (latest.Tag == "" || r.Published.After(latest.Published)) {
				latest = r
			}
		}
		if len(releases) < 100 {
			if latest.Tag == "" {
				return latest, fmt.Errorf("no published GitHub release")
			}
			return latest, nil
		}
	}
	return release{}, fmt.Errorf("too many releases to determine latest safely")
}
func (r release) asset(name string) (asset, error) {
	var found asset
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		if found.Name != "" {
			return asset{}, fmt.Errorf("duplicate release asset %s", name)
		}
		found = a
	}
	if found.URL == "" && found.DownloadURL == "" {
		return found, fmt.Errorf("release %s is missing asset %s", r.Tag, name)
	}
	return found, nil
}
func checksum(text, name string) ([]byte, error) {
	var result []byte
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum, err := hex.DecodeString(fields[0])
		if err != nil || len(sum) != sha256.Size || result != nil {
			return nil, fmt.Errorf("invalid or duplicate SHA256SUMS entry for %s", name)
		}
		result = sum
	}
	if result == nil {
		return nil, fmt.Errorf("SHA256SUMS has no entry for %s", name)
	}
	return result, nil
}
func (g github) download(ctx context.Context, r release, directory, arch string, names []string) error {
	packageName := "project-alpha_" + r.Tag + "_linux_" + arch
	archiveName := packageName + ".tar.gz"
	archive, err := r.asset(archiveName)
	if err != nil {
		return err
	}
	sums, err := r.asset("SHA256SUMS")
	if err != nil {
		return err
	}
	var b strings.Builder
	if err = g.downloadAsset(ctx, sums, &b, 1<<20); err != nil {
		return err
	}
	expected, err := checksum(b.String(), archiveName)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(directory, "release.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if err = g.downloadAsset(ctx, archive, io.MultiWriter(f, hash), maxArchive); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != hex.EncodeToString(expected) {
		return fmt.Errorf("SHA-256 mismatch for %s; nothing installed", archiveName)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extract(f, directory, packageName, names)
}
func extract(src io.Reader, directory, packageName string, names []string) error {
	gz, err := gzip.NewReader(src)
	if err != nil {
		return err
	}
	defer gz.Close()
	// Bound decompression even for ignored documentation entries.
	tr := tar.NewReader(io.LimitReader(gz, 1<<30))
	wanted := map[string]string{}
	for _, name := range names {
		wanted[packageName+"/bin/"+name] = name
	}
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		clean := path.Clean(h.Name)
		if path.IsAbs(h.Name) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(h.Name, "\\") {
			return fmt.Errorf("unsafe archive entry %q", h.Name)
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
			return fmt.Errorf("unsupported archive entry %q", h.Name)
		}
		name, ok := wanted[clean]
		if !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg || seen[name] || h.Size <= 0 || h.Size > maxBinary {
			return fmt.Errorf("invalid binary entry %q", h.Name)
		}
		f, e := os.OpenFile(filepath.Join(directory, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
		if e != nil {
			return e
		}
		_, copyErr := io.Copy(f, tr)
		modeErr := f.Chmod(0755) // main uses umask 0077; service accounts still need to execute installed binaries.
		syncErr := f.Sync()
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if modeErr != nil {
			return modeErr
		}
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
		seen[name] = true
	}
	for _, name := range names {
		if !seen[name] {
			return fmt.Errorf("release package missing bin/%s", name)
		}
	}
	return nil
}

// A validated webhook already establishes the published tag and channel. For
// public releases the package naming convention is enough; no API lookup needed.
func (g github) publicRelease(tag, arch string) release {
	base := "https://github.com/" + g.repo + "/releases/download/" + url.PathEscape(tag) + "/"
	name := "project-alpha_" + tag + "_linux_" + arch + ".tar.gz"
	return release{Tag: tag, Assets: []asset{
		{Name: name, DownloadURL: base + name},
		{Name: "SHA256SUMS", DownloadURL: base + "SHA256SUMS"},
	}}
}
func (g github) byTag(ctx context.Context, tag string, prerelease bool) (release, error) {
	var b strings.Builder
	var r release
	if err := g.get(ctx, g.base+"/repos/"+g.repo+"/releases/tags/"+url.PathEscape(tag), "application/vnd.github+json", &b, 4<<20); err != nil {
		return r, err
	}
	if err := json.Unmarshal([]byte(b.String()), &r); err != nil {
		return r, err
	}
	if r.Tag != tag || r.Draft || r.Prerelease && !prerelease {
		return r, fmt.Errorf("release is not eligible for the configured channel")
	}
	return r, nil
}
