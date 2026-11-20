package core

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type nativeStreamAsset struct {
	address     string
	localPath   string
	data        []byte
	contentType string
	total       int64
	etag        string
	modified    string
}

type nativeStreamSession struct {
	credentials *providerMediaCredentials
	mu          sync.Mutex
	assets      map[string]nativeStreamAsset
	referer     string
	key         []byte
	cencKey     []byte
	localDir    string
	ctx         context.Context
	cancel      context.CancelFunc
	lastUsed    time.Time
}

type nativeStreamServer struct {
	mu           sync.Mutex
	downloader   *Downloader
	address      string
	sessions     map[string]*nativeStreamSession
	server       *http.Server
	cencMu       sync.Mutex
	cencMeta     map[string]*cencStreamMetadata
	cencInflight map[string]chan struct{}
}

func (stream *nativeStreamServer) nativeRequest(request *http.Request) (*http.Response, error) {
	client := *stream.downloader.client
	client.Timeout = 0
	return stream.downloader.doMediaRequestWithClient(request, &client)
}

var nativePlaylistURI = regexp.MustCompile(`URI="([^"]+)"`)

func newNativeStreamServer(d *Downloader) (*nativeStreamServer, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("无法初始化本机播放器")
	}
	stream := &nativeStreamServer{
		downloader:   d,
		address:      "http://" + listener.Addr().String(),
		sessions:     map[string]*nativeStreamSession{},
		cencMeta:     map[string]*cencStreamMetadata{},
		cencInflight: map[string]chan struct{}{},
	}
	server := &http.Server{Handler: http.HandlerFunc(stream.nativeServe), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	stream.server = server
	go func() { _ = server.Serve(listener) }()
	return stream, nil
}

func (stream *nativeStreamServer) registerSession(session *nativeStreamSession) string {
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		panic(err)
	}
	token := hex.EncodeToString(tokenBytes)
	var evicted []string
	stream.mu.Lock()
	for id, old := range stream.sessions {
		if time.Since(old.lastUsed) > 10*time.Minute {
			old.cancel()
			delete(stream.sessions, id)
			evicted = append(evicted, id)
		}
	}
	if len(stream.sessions) >= 8 {
		oldest := ""
		for id, old := range stream.sessions {
			if oldest == "" || old.lastUsed.Before(stream.sessions[oldest].lastUsed) {
				oldest = id
			}
		}
		stream.sessions[oldest].cancel()
		delete(stream.sessions, oldest)
		evicted = append(evicted, oldest)
	}
	stream.sessions[token] = session
	stream.mu.Unlock()
	for _, id := range evicted {
		stream.releaseCENC(id)
	}
	return token
}

func (stream *nativeStreamServer) nativeOpen(media providerMedia) (string, string) {
	ctx, cancel := context.WithCancel(providerMediaContext(context.Background(), media.credentials))
	var cencKey []byte
	if len(media.CENCKey) == 16 {
		cencKey = append([]byte(nil), media.CENCKey...)
	}
	session := &nativeStreamSession{
		assets:      map[string]nativeStreamAsset{},
		referer:     media.Referer,
		key:         media.HLSKey,
		cencKey:     cencKey,
		ctx:         ctx,
		cancel:      cancel,
		lastUsed:    time.Now(),
		credentials: media.credentials,
	}
	token := stream.registerSession(session)
	entry := nativeStreamAsset{address: media.URL, contentType: "video/mp4"}
	parsedURL, parseErr := url.Parse(media.URL)
	isHLS := media.Playlist != "" || len(media.HLSKey) > 0 || (parseErr == nil && strings.HasSuffix(strings.ToLower(parsedURL.Path), ".m3u8"))
	if isHLS {
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	if media.Playlist != "" {
		entry.data = []byte(media.Playlist)
		entry.contentType = "application/vnd.apple.mpegurl"
	}
	return stream.nativeAsset(token, session, entry), token
}

func (stream *nativeStreamServer) nativeOpenLocal(filePath string, keyHex string) (string, string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(filePath))
	if cleaned == "" || cleaned == "." {
		return "", "", errors.New("本地媒体路径无效")
	}
	info, err := os.Stat(cleaned)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", errNativeLocalFile
	}
	cencKey, err := decodeCENCKeyHex(keyHex)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithCancel(context.Background())
	session := &nativeStreamSession{
		assets:   map[string]nativeStreamAsset{},
		cencKey:  cencKey,
		localDir: filepath.Dir(cleaned),
		ctx:      ctx,
		cancel:   cancel,
		lastUsed: time.Now(),
	}
	token := stream.registerSession(session)
	contentType := "video/mp4"
	if strings.EqualFold(filepath.Ext(cleaned), ".m3u8") {
		contentType = "application/vnd.apple.mpegurl"
	}
	entry := nativeStreamAsset{
		address:     cleaned,
		localPath:   cleaned,
		contentType: contentType,
	}
	return stream.nativeAsset(token, session, entry), token, nil
}

func (stream *nativeStreamServer) nativeRelease(token string) {
	stream.mu.Lock()
	if session := stream.sessions[token]; session != nil {
		session.cancel()
		delete(stream.sessions, token)
	}
	stream.mu.Unlock()
	stream.releaseCENC(token)
}

func (stream *nativeStreamServer) nativeAsset(token string, session *nativeStreamSession, asset nativeStreamAsset) string {
	identity := asset.address
	if asset.localPath != "" {
		identity = "local:" + asset.localPath
	}
	digest := sha256.Sum256([]byte(identity + "\x00" + asset.contentType))
	extension := ".ts"
	if asset.localPath != "" {
		switch candidate := strings.ToLower(filepath.Ext(asset.localPath)); candidate {
		case ".m3u8", ".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".vtt", ".webvtt", ".key":
			extension = candidate
		}
	} else if parsed, err := url.Parse(asset.address); err == nil {
		switch candidate := strings.ToLower(path.Ext(parsed.Path)); candidate {
		case ".m3u8", ".ts", ".m4s", ".mp4", ".aac", ".m4a", ".mp3", ".vtt", ".webvtt", ".key":
			extension = candidate
		}
	}
	switch asset.contentType {
	case "application/vnd.apple.mpegurl":
		extension = ".m3u8"
	case "application/octet-stream":
		extension = ".key"
	case "video/mp4":
		extension = ".mp4"
	}
	id := hex.EncodeToString(digest[:12]) + extension
	session.mu.Lock()
	if previous, found := session.assets[id]; !found || len(previous.data) == 0 || len(asset.data) > 0 {
		session.assets[id] = asset
	}
	session.mu.Unlock()
	return stream.address + "/" + token + "/" + id
}

func (stream *nativeStreamServer) nativeRewrite(token string, session *nativeStreamSession, body, base string) (string, error) {
	var output []string
	nextPlaylist := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		text := strings.TrimSpace(line)
		rewrite := func(reference string, key bool, contentType string) string {
			if strings.HasPrefix(reference, "data:") {
				return reference
			}
			parsed, err := url.Parse(base)
			if err != nil {
				return ""
			}
			relative, err := url.Parse(reference)
			if err != nil {
				return ""
			}
			address := parsed.ResolveReference(relative).String()
			if !isProviderHTTPMediaURL(address) {
				return ""
			}
			asset := nativeStreamAsset{address: address, contentType: contentType}
			if key && len(session.key) == 16 {
				asset.data = append([]byte{}, session.key...)
				asset.contentType = "application/octet-stream"
			}
			return stream.nativeAsset(token, session, asset)
		}
		if text != "" && !strings.HasPrefix(text, "#") {
			contentType := ""
			if nextPlaylist {
				contentType = "application/vnd.apple.mpegurl"
			}
			line = rewrite(text, false, contentType)
			nextPlaylist = false
			if line == "" {
				return "", errors.New("播放列表中的媒体地址无效")
			}
		} else if strings.Contains(text, "URI=") {
			invalid := false
			line = nativePlaylistURI.ReplaceAllStringFunc(line, func(match string) string {
				reference := nativePlaylistURI.FindStringSubmatch(match)[1]
				key := strings.HasPrefix(text, "#EXT-X-KEY:") || strings.HasPrefix(text, "#EXT-X-SESSION-KEY:")
				contentType := ""
				switch {
				case key:
					contentType = "application/octet-stream"
				case strings.HasPrefix(text, "#EXT-X-MAP:"):
					contentType = "video/mp4"
				case strings.HasPrefix(text, "#EXT-X-MEDIA:"), strings.HasPrefix(text, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(text, "#EXT-X-RENDITION-REPORT:"):
					contentType = "application/vnd.apple.mpegurl"
				}
				updated := rewrite(reference, key, contentType)
				if updated == "" {
					invalid = true
				}
				return "URI=\"" + updated + "\""
			})
			if invalid {
				return "", errors.New("播放列表中的附属地址无效")
			}
		}
		if strings.HasPrefix(text, "#EXT-X-STREAM-INF:") {
			nextPlaylist = true
		}
		output = append(output, line)
	}
	return strings.Join(output, "\n"), nil
}

func (stream *nativeStreamServer) nativeRewriteLocal(token string, session *nativeStreamSession, body, playlistPath string) (string, error) {
	baseDir := filepath.Dir(playlistPath)
	rootClean := filepath.Clean(session.localDir)
	resolveLocal := func(reference string, contentType string) string {
		if strings.HasPrefix(reference, "data:") {
			return reference
		}
		ref := strings.TrimSpace(reference)
		if idx := strings.IndexAny(ref, "?#"); idx >= 0 {
			ref = ref[:idx]
		}
		if ref == "" || strings.Contains(ref, "://") {
			return ""
		}
		joined := filepath.Clean(filepath.Join(baseDir, filepath.FromSlash(ref)))
		rel, err := filepath.Rel(rootClean, joined)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ""
		}
		asset := nativeStreamAsset{address: joined, localPath: joined, contentType: contentType}
		return stream.nativeAsset(token, session, asset)
	}
	var output []string
	nextPlaylist := false
	for _, line := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		text := strings.TrimSpace(line)
		if text != "" && !strings.HasPrefix(text, "#") {
			contentType := ""
			if nextPlaylist || strings.HasSuffix(strings.ToLower(text), ".m3u8") {
				contentType = "application/vnd.apple.mpegurl"
			}
			line = resolveLocal(text, contentType)
			nextPlaylist = false
			if line == "" {
				return "", errors.New("本地播放列表中的分片路径无效")
			}
		} else if strings.Contains(text, "URI=") {
			invalid := false
			line = nativePlaylistURI.ReplaceAllStringFunc(line, func(match string) string {
				reference := nativePlaylistURI.FindStringSubmatch(match)[1]
				contentType := ""
				switch {
				case strings.HasPrefix(text, "#EXT-X-KEY:"), strings.HasPrefix(text, "#EXT-X-SESSION-KEY:"):
					contentType = "application/octet-stream"
				case strings.HasPrefix(text, "#EXT-X-MAP:"):
					contentType = "video/mp4"
				case strings.HasPrefix(text, "#EXT-X-MEDIA:"), strings.HasPrefix(text, "#EXT-X-I-FRAME-STREAM-INF:"), strings.HasPrefix(text, "#EXT-X-RENDITION-REPORT:"):
					contentType = "application/vnd.apple.mpegurl"
				}
				updated := resolveLocal(reference, contentType)
				if updated == "" {
					invalid = true
				}
				return "URI=\"" + updated + "\""
			})
			if invalid {
				return "", errors.New("本地播放列表中的附属路径无效")
			}
		}
		if strings.HasPrefix(text, "#EXT-X-STREAM-INF:") {
			nextPlaylist = true
		}
		output = append(output, line)
	}
	return strings.Join(output, "\n"), nil
}

func hasDisguisedMediaHeader(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	return bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) ||
		bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}) ||
		bytes.HasPrefix(data, []byte("GIF8")) ||
		bytes.HasPrefix(data, []byte("BM")) ||
		bytes.HasPrefix(data, []byte("RIFF")) ||
		bytes.HasPrefix(data, []byte("ID3")) ||
		bytes.HasPrefix(data, []byte{0xef, 0xbb, 0xbf})
}

func disguisedTSPrefixOffset(data []byte) int {
	if len(data) == 0 || data[0] == 0x47 {
		return 0
	}
	maxScan := min(len(data), 4096)
	for offset := 1; offset+564 < len(data) && offset <= maxScan; offset++ {
		if data[offset] == 0x47 && data[offset+188] == 0x47 && data[offset+376] == 0x47 && data[offset+564] == 0x47 {
			return offset
		}
	}
	if hasDisguisedMediaHeader(data) {
		for offset := 1; offset+376 < len(data) && offset <= maxScan; offset++ {
			if data[offset] == 0x47 && data[offset+188] == 0x47 && data[offset+376] == 0x47 {
				return offset
			}
		}
		for offset := 1; offset+188 <= len(data) && offset <= maxScan; offset++ {
			if data[offset] == 0x47 && (offset+188 == len(data) || data[offset+188] == 0x47) {
				return offset
			}
		}
	}
	return 0
}

func (stream *nativeStreamServer) serveLocalAsset(ctx context.Context, writer http.ResponseWriter, request *http.Request, token, assetID string, session *nativeStreamSession, asset nativeStreamAsset) {
	if len(session.cencKey) == 16 && !strings.Contains(asset.contentType, "mpegurl") {
		stream.serveDecryptedCENC(ctx, writer, request, token+"/"+assetID, session, asset)
		return
	}
	if strings.Contains(asset.contentType, "mpegurl") {
		raw, err := os.ReadFile(asset.localPath)
		if err != nil {
			http.Error(writer, "无法读取本地播放列表", http.StatusNotFound)
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(string(raw), "\ufeff"))
		rewritten, err := stream.nativeRewriteLocal(token, session, text, asset.localPath)
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		if request.Method == http.MethodGet {
			_, _ = io.WriteString(writer, rewritten)
		}
		return
	}
	file, err := os.Open(asset.localPath)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	defer file.Close()
	if request.Method == http.MethodGet && request.Header.Get("Range") == "" && strings.EqualFold(filepath.Ext(asset.localPath), ".ts") {
		probe := make([]byte, 4096)
		n, _ := file.ReadAt(probe, 0)
		if offset := disguisedTSPrefixOffset(probe[:n]); offset > 0 {
			if info, statErr := file.Stat(); statErr == nil && info.Size() > int64(offset) {
				writer.Header().Set("Content-Type", "video/mp2t")
				http.ServeContent(writer, request, assetID, info.ModTime(), io.NewSectionReader(file, int64(offset), info.Size()-int64(offset)))
				return
			}
		}
	}
	if asset.contentType != "" {
		writer.Header().Set("Content-Type", asset.contentType)
	}
	info, err := file.Stat()
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	http.ServeContent(writer, request, assetID, info.ModTime(), file)
}

func (stream *nativeStreamServer) nativeServe(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) != 2 {
		http.NotFound(writer, request)
		return
	}
	stream.mu.Lock()
	session := stream.sessions[parts[0]]
	if session != nil {
		session.lastUsed = time.Now()
	}
	stream.mu.Unlock()
	if session == nil {
		http.Error(writer, "播放已结束", http.StatusGone)
		return
	}
	session.mu.Lock()
	asset, found := session.assets[parts[1]]
	session.mu.Unlock()
	if !found {
		http.NotFound(writer, request)
		return
	}
	ctx, cancel := context.WithCancel(providerMediaContext(request.Context(), session.credentials))
	defer cancel()
	stop := context.AfterFunc(session.ctx, cancel)
	defer stop()
	writer.Header().Set("Cache-Control", "no-store")
	if asset.localPath != "" {
		stream.serveLocalAsset(ctx, writer, request, parts[0], parts[1], session, asset)
		return
	}
	if len(session.cencKey) == 16 && !strings.Contains(asset.contentType, "mpegurl") {
		stream.serveDecryptedCENC(ctx, writer, request, parts[0]+"/"+parts[1], session, asset)
		return
	}
	if len(asset.data) > 0 && asset.total > int64(len(asset.data)) {
		if stream.nativeServePrefix(ctx, writer, request, session, asset) {
			return
		}
		asset.data = nil
	}
	if len(asset.data) > 0 {
		if strings.Contains(asset.contentType, "mpegurl") {
			body, err := stream.nativeRewrite(parts[0], session, string(asset.data), asset.address)
			if err != nil {
				http.Error(writer, err.Error(), http.StatusBadGateway)
				return
			}
			writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			if request.Method == http.MethodGet {
				_, _ = io.WriteString(writer, body)
			}
		} else {
			writer.Header().Set("Content-Type", asset.contentType)
			if asset.etag != "" {
				writer.Header().Set("ETag", asset.etag)
			}
			http.ServeContent(writer, request, parts[1], time.Time{}, bytes.NewReader(asset.data))
		}
		return
	}
	upstream, err := http.NewRequestWithContext(ctx, request.Method, asset.address, nil)
	if err != nil {
		http.Error(writer, "媒体地址无效", http.StatusBadGateway)
		return
	}
	upstream.Header.Set("User-Agent", userAgent)
	upstream.Header.Set("Referer", session.referer)
	for _, name := range []string{"Range", "If-Range"} {
		if value := request.Header.Get(name); value != "" {
			upstream.Header.Set(name, value)
		}
	}
	response, err := stream.nativeRequest(upstream)
	if err != nil {
		http.Error(writer, "读取媒体失败，请重试", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		err := stream.downloader.catalogResponseError(upstream, response, body)
		http.Error(writer, err.Error(), response.StatusCode)
		return
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	finalURL := upstream.URL
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL
	}
	contentIsMP4 := strings.Contains(contentType, "video/mp4")
	playlist := (strings.Contains(asset.contentType, "mpegurl") && !contentIsMP4) ||
		strings.Contains(contentType, "mpegurl") ||
		(strings.HasSuffix(strings.ToLower(finalURL.Path), ".m3u8") && !contentIsMP4)
	reader := bufio.NewReaderSize(response.Body, 8192)
	if request.Method == http.MethodGet {
		peek, _ := reader.Peek(512)
		bodyIsPlaylist := strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(string(peek), "\ufeff")), "#EXTM3U")
		if bodyIsPlaylist {
			playlist = true
		} else if contentIsMP4 || (!strings.Contains(contentType, "mpegurl") && !strings.HasSuffix(strings.ToLower(finalURL.Path), ".m3u8")) {
			playlist = false
		}
	}
	if playlist && request.Method == http.MethodHead {
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		writer.WriteHeader(http.StatusOK)
		return
	}
	if playlist && request.Method == http.MethodGet {
		body, err := io.ReadAll(io.LimitReader(reader, 4<<20+1))
		if err != nil || len(body) > 4<<20 {
			http.Error(writer, "播放列表过大或读取失败", http.StatusBadGateway)
			return
		}
		text := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
		if !strings.HasPrefix(text, "#EXTM3U") {
			http.Error(writer, "播放列表无效", http.StatusBadGateway)
			return
		}
		rewritten, err := stream.nativeRewrite(parts[0], session, text, finalURL.String())
		if err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
		writer.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = io.WriteString(writer, rewritten)
		return
	}

	stripOffset := 0
	if request.Method == http.MethodGet &&
		request.Header.Get("Range") == "" &&
		!contentIsMP4 &&
		asset.contentType != "application/octet-stream" &&
		!strings.HasSuffix(strings.ToLower(parts[1]), ".key") {
		peek, _ := reader.Peek(4096)
		if offset := disguisedTSPrefixOffset(peek); offset > 0 {
			if _, dErr := reader.Discard(offset); dErr == nil {
				stripOffset = offset
			}
		}
	}

	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if value := response.Header.Get(name); value != "" {
			if stripOffset > 0 {
				if name == "Content-Type" {
					writer.Header().Set("Content-Type", "video/mp2t")
					continue
				}
				if name == "Content-Length" {
					if total, err := strconv.ParseInt(value, 10, 64); err == nil && total > int64(stripOffset) {
						writer.Header().Set("Content-Length", strconv.FormatInt(total-int64(stripOffset), 10))
					}
					continue
				}
			}
			writer.Header().Set(name, value)
		}
	}
	if stripOffset > 0 && writer.Header().Get("Content-Type") == "" {
		writer.Header().Set("Content-Type", "video/mp2t")
	}
	writer.WriteHeader(response.StatusCode)
	if request.Method == http.MethodGet {
		_, _ = io.Copy(writer, reader)
	}
}
