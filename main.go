package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultPort       = "8080"
	channelsFile      = "channels.m3u"
	maxPlaylistBytes  = 10 << 20
	maxRedirects      = 5
	upstreamUserAgent = "light-m3u-proxy/0.1"
	copyBufferSize    = 32 << 10
	maxLogURLLength   = 180
)

var (
	publicBaseURL string
	httpClient    *http.Client
)

func main() {
	publicBaseURL = strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/")
	if publicBaseURL == "" {
		log.Fatal("PUBLIC_BASE_URL is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	httpClient = newHTTPClient()

	mux := http.NewServeMux()
	mux.HandleFunc("/iptv.m3u", playlistHandler)
	mux.HandleFunc("/proxy", proxyHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("light-m3u-proxy listening on :%s, fallback public base url: %s", port, publicBaseURL)
	log.Fatal(server.ListenAndServe())
}

func newHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func playlistHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setCommonHeaders(w.Header())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	file, err := os.Open(channelsFile)
	if err != nil {
		http.Error(w, "channels.m3u not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	setM3U8Headers(w.Header())
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxPlaylistBytes)
	baseURL := requestBaseURL(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if isHTTPURL(line) {
			fmt.Fprintln(w, proxyURL(baseURL, line))
			continue
		}
		fmt.Fprintln(w, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		log.Printf("playlist read error: %v", err)
	}
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		setCommonHeaders(w.Header())
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}

	target, err := url.Parse(raw)
	if err != nil {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	if err := validateTargetURL(target); err != nil {
		log.Printf("invalid proxy target raw_url=%s err=%v", truncateLog(raw), err)
		http.Error(w, "invalid target url", http.StatusBadRequest)
		return
	}
	log.Printf("proxy request client_method=%s raw_url=%s", r.Method, truncateLog(raw))

	start := time.Now()
	resp, finalURL, redirects, err := fetchUpstream(r, target)
	if err != nil {
		log.Printf("upstream error url=%s err=%v", logURL(target), err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	upstreamContentType := resp.Header.Get("Content-Type")
	logRedirectChain(r.Method, redirects)
	log.Printf("proxy final client_method=%s upstream_method=GET final_url=%s final_status=%d content-type=%q redirects=%d", r.Method, logURL(finalURL), resp.StatusCode, upstreamContentType, len(redirects))

	if isM3U8(finalURL, upstreamContentType) {
		serveM3U8(w, r, resp, finalURL, upstreamContentType, start)
		return
	}

	copyResponseHeaders(w.Header(), resp.Header, true)
	setBinaryHeaders(w.Header(), finalURL, upstreamContentType)
	w.WriteHeader(resp.StatusCode)
	if r.Method == http.MethodHead {
		log.Printf("proxy binary client_method=%s upstream_method=GET final_url=%s status=%d content-type=%q head_only=true", r.Method, logURL(finalURL), resp.StatusCode, w.Header().Get("Content-Type"))
		return
	}

	n, err := streamCopy(w, resp.Body)
	if err != nil {
		log.Printf("stream copy error status=%d bytes=%d duration=%s url=%s err=%v", resp.StatusCode, n, time.Since(start).Round(time.Millisecond), logURL(finalURL), err)
		return
	}
	log.Printf("proxy binary client_method=%s upstream_method=GET final_url=%s status=%d content-type=%q bytes=%d duration=%s", r.Method, logURL(finalURL), resp.StatusCode, w.Header().Get("Content-Type"), n, time.Since(start).Round(time.Millisecond))
}

type redirectHop struct {
	From   *url.URL
	To     *url.URL
	Status int
}

type m3u8Result struct {
	body        string
	lines       int
	status      int
	headers     http.Header
	finalURL    *url.URL
	contentType string
	flattened   bool
	flattenErr  string
}

func serveM3U8(w http.ResponseWriter, r *http.Request, resp *http.Response, finalURL *url.URL, contentType string, start time.Time) {
	if r.Method == http.MethodHead {
		copyResponseHeaders(w.Header(), resp.Header, false)
		setM3U8Headers(w.Header())
		w.WriteHeader(resp.StatusCode)
		log.Printf("proxy m3u8 summary client_method=%s final_url=%s final_status=%d content-type=%q m3u8_rewrite=false rewrite_lines=0 flattened=false head_only=true", r.Method, logURL(finalURL), resp.StatusCode, contentType)
		return
	}

	body, err := readLimitedText(resp.Body)
	if err != nil {
		log.Printf("m3u8 read error url=%s err=%v", logURL(finalURL), err)
		return
	}

	baseURL := requestBaseURL(r)
	result := rewritePlaylist(body, finalURL, baseURL, resp.StatusCode, resp.Header, contentType)
	if childURI, ok := singleVariantURI(body); ok {
		if flattened, err := flattenVariantPlaylist(r, childURI, finalURL, baseURL); err != nil {
			result.flattenErr = err.Error()
		} else {
			result = flattened
			result.flattened = true
		}
	}

	copyResponseHeaders(w.Header(), result.headers, false)
	setM3U8Headers(w.Header())
	w.WriteHeader(result.status)
	_, _ = io.WriteString(w, result.body)

	if result.flattenErr != "" {
		log.Printf("proxy flatten summary flattened=false flatten_error=%q original_url=%s", truncateLog(result.flattenErr), logURL(finalURL))
	}
	log.Printf("proxy m3u8 rewrite summary client_method=%s final_url=%s final_status=%d content-type=%q m3u8_rewrite=true rewrite_lines=%d flattened=%t bytes=%d duration=%s", r.Method, logURL(result.finalURL), result.status, result.contentType, result.lines, result.flattened, len(result.body), time.Since(start).Round(time.Millisecond))
}

func flattenVariantPlaylist(r *http.Request, childRaw string, base *url.URL, requestBase string) (m3u8Result, error) {
	childResolved, ok := resolvePlaylistURL(childRaw, base)
	if !ok {
		return m3u8Result{}, errors.New("variant child url is not http/https")
	}
	childURL, err := url.Parse(childResolved)
	if err != nil {
		return m3u8Result{}, err
	}

	childResp, childFinalURL, childRedirects, err := fetchUpstream(r, childURL)
	if err != nil {
		return m3u8Result{}, err
	}
	defer childResp.Body.Close()

	logRedirectChain(r.Method, childRedirects)
	childContentType := childResp.Header.Get("Content-Type")
	if !isM3U8(childFinalURL, childContentType) {
		return m3u8Result{}, fmt.Errorf("child is not m3u8: status=%d content-type=%q", childResp.StatusCode, childContentType)
	}

	body, err := readLimitedText(childResp.Body)
	if err != nil {
		return m3u8Result{}, err
	}
	return rewritePlaylist(body, childFinalURL, requestBase, childResp.StatusCode, childResp.Header, childContentType), nil
}

func readLimitedText(r io.Reader) (string, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxPlaylistBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxPlaylistBytes {
		return "", errors.New("m3u8 too large")
	}
	return string(body), nil
}

func rewritePlaylist(body string, base *url.URL, requestBase string, status int, headers http.Header, contentType string) m3u8Result {
	rewritten, lines := rewriteM3U8(body, base, requestBase)
	return m3u8Result{
		body:        rewritten,
		lines:       lines,
		status:      status,
		headers:     headers,
		finalURL:    base,
		contentType: contentType,
	}
}

func fetchUpstream(r *http.Request, target *url.URL) (*http.Response, *url.URL, []redirectHop, error) {
	current := cloneURL(target)
	var redirects []redirectHop

	for {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, current.String(), nil)
		if err != nil {
			return nil, current, redirects, err
		}
		req.Header.Set("User-Agent", upstreamUserAgent)
		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, current, redirects, err
		}

		if !isRedirectStatus(resp.StatusCode) {
			return resp, current, redirects, nil
		}
		if len(redirects) >= maxRedirects {
			resp.Body.Close()
			return nil, current, redirects, errors.New("too many upstream redirects")
		}

		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			resp.Body.Close()
			return nil, current, redirects, errors.New("upstream redirect missing location")
		}
		resolvedRaw, ok := resolvePlaylistURL(location, current)
		if !ok {
			resp.Body.Close()
			return nil, current, redirects, errors.New("upstream redirect location is not http/https")
		}
		resolved, err := url.Parse(resolvedRaw)
		if err != nil {
			resp.Body.Close()
			return nil, current, redirects, err
		}
		if err := validateTargetURL(resolved); err != nil {
			resp.Body.Close()
			return nil, current, redirects, err
		}

		redirects = append(redirects, redirectHop{
			From:   cloneURL(current),
			To:     cloneURL(resolved),
			Status: resp.StatusCode,
		})
		resp.Body.Close()

		current = resolved
	}
}

func logRedirectChain(clientMethod string, redirects []redirectHop) {
	if len(redirects) == 0 {
		log.Printf("proxy redirect chain summary client_method=%s upstream_method=GET redirects=0", clientMethod)
		return
	}
	for i, hop := range redirects {
		log.Printf("proxy redirect chain client_method=%s upstream_method=GET hop=%d status=%d from=%s to=%s", clientMethod, i+1, hop.Status, logURL(hop.From), logURL(hop.To))
	}
	log.Printf("proxy redirect chain summary client_method=%s upstream_method=GET redirects=%d final=%s", clientMethod, len(redirects), logURL(redirects[len(redirects)-1].To))
}

func validateTargetURL(u *url.URL) error {
	if u == nil || u.String() == "" {
		return errors.New("empty url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("only http and https are allowed")
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("missing host")
	}
	return nil
}

func isRedirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

func rewriteM3U8(body string, base *url.URL, requestBase string) (string, int) {
	lines := strings.Split(body, "\n")
	rewrittenLines := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			continue
		}

		if resolved, ok := resolvePlaylistURL(trimmed, base); ok {
			lines[i] = proxyURL(requestBase, resolved)
			rewrittenLines++
		}
	}
	return strings.Join(lines, "\n"), rewrittenLines
}

func singleVariantURI(body string) (string, bool) {
	lines := strings.Split(body, "\n")
	hasStreamInf := false
	var uris []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF") {
				hasStreamInf = true
			}
			continue
		}
		uris = append(uris, trimmed)
	}

	if hasStreamInf && len(uris) == 1 {
		return uris[0], true
	}
	return "", false
}

func resolvePlaylistURL(raw string, base *url.URL) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	if u.IsAbs() {
		if u.Scheme == "http" || u.Scheme == "https" {
			return u.String(), true
		}
		return "", false
	}
	return base.ResolveReference(u).String(), true
}

func proxyURL(baseURL, raw string) string {
	return baseURL + "/proxy?url=" + url.QueryEscape(raw)
}

func requestBaseURL(r *http.Request) string {
	scheme := firstForwardedValue(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	if scheme != "http" && scheme != "https" {
		return publicBaseURL
	}

	host := firstForwardedValue(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = strings.TrimSpace(r.Host)
	}
	if host == "" {
		return publicBaseURL
	}

	if !hostHasPort(host) {
		if port := firstForwardedValue(r.Header.Get("X-Forwarded-Port")); port != "" {
			host = net.JoinHostPort(strings.Trim(host, "[]"), port)
		}
	}

	return scheme + "://" + host
}

func firstForwardedValue(value string) string {
	if value == "" {
		return ""
	}
	value = strings.Split(value, ",")[0]
	return strings.TrimSpace(value)
}

func hostHasPort(host string) bool {
	if strings.TrimSpace(host) == "" {
		return false
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return true
	}
	if strings.Count(host, ":") == 1 && strings.LastIndex(host, ":") > 0 {
		return true
	}
	return false
}

func isHTTPURL(line string) bool {
	return strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://")
}

func isM3U8(u *url.URL, contentType string) bool {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(mediaType) {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl":
		return true
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}

func copyResponseHeaders(dst, src http.Header, includeContentLength bool) {
	for _, key := range []string{
		"Content-Type",
		"Accept-Ranges",
		"Content-Range",
		"Cache-Control",
		"Last-Modified",
		"ETag",
	} {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
	if includeContentLength {
		if value := src.Get("Content-Length"); value != "" {
			dst.Set("Content-Length", value)
		}
	}
}

func setCommonHeaders(h http.Header) {
	h.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	h.Set("Pragma", "no-cache")
	h.Set("Expires", "0")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "*")
}

func setM3U8Headers(h http.Header) {
	setCommonHeaders(h)
	h.Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
}

func setBinaryHeaders(h http.Header, target *url.URL, upstreamContentType string) {
	setCommonHeaders(h)
	if strings.HasSuffix(strings.ToLower(target.Path), ".ts") {
		h.Set("Content-Type", "video/mp2t")
		return
	}
	if upstreamContentType != "" {
		h.Set("Content-Type", upstreamContentType)
	}
}

func streamCopy(w http.ResponseWriter, r io.Reader) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var written int64
	flusher, _ := w.(http.Flusher)
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if flusher != nil {
				flusher.Flush()
			}
			if ew != nil {
				return written, ew
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
		}
		if er != nil {
			if er == io.EOF {
				return written, nil
			}
			return written, er
		}
	}
}

func scrubURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	clean := *u
	clean.RawQuery = ""
	return clean.String()
}

func logURL(u *url.URL) string {
	return truncateLog(scrubURL(u))
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copied := *u
	return &copied
}

func truncateLog(value string) string {
	if len(value) <= maxLogURLLength {
		return value
	}
	return value[:maxLogURLLength] + "..."
}
