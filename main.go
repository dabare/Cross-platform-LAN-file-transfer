package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	discoveryPort = 32997
	defaultHTTP   = 32998
	peerTTL       = 45 * time.Second
	maxEditBytes  = 5 << 20
)

//go:embed static/*
var staticFiles embed.FS

type app struct {
	id        string
	name      string
	root      string
	port      int
	baseURL   string
	started   time.Time
	peersMu   sync.RWMutex
	peers     map[string]peer
	clients   map[string]peer
	client    *http.Client
	transfer  *http.Client
	xfersMu   sync.RWMutex
	xfers     map[string]*transferState
	clientMu  sync.Mutex
	events    map[string]map[chan clientEvent]bool
	pending   map[string][]clientEvent
	downloads map[string]browserDownload
	scanMu    sync.Mutex
	scanTime  time.Time
}

type peer struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	URL        string    `json:"url"`
	OS         string    `json:"os"`
	Kind       string    `json:"kind"`
	CanReceive bool      `json:"canReceive"`
	LastSeen   time.Time `json:"lastSeen"`
}

type discoveryMessage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
	OS   string `json:"os"`
}

type fileEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type transferState struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Direction string    `json:"direction"`
	Bytes     int64     `json:"bytes"`
	Total     int64     `json:"total"`
	Status    string    `json:"status"`
	Started   time.Time `json:"started"`
	Updated   time.Time `json:"updated"`
}

type clientEvent struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

type browserDownload struct {
	ID       string
	ClientID string
	Name     string
	Path     string
	Size     int64
	Created  time.Time
}

type progressReader struct {
	reader io.Reader
	onRead func(int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.reader.Read(buf)
	if n > 0 {
		p.onRead(int64(n))
	}
	return n, err
}

func main() {
	port := flag.Int("port", defaultHTTP, "HTTP port")
	noOpen := flag.Bool("no-open", false, "do not open the web browser")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "file-transfer"
	}
	id := loadOrCreateID(root)
	a := &app{
		id:      id,
		name:    hostname,
		root:    root,
		port:    *port,
		baseURL: fmt.Sprintf("http://%s:%d", localAdvertiseIP(), *port),
		started: time.Now(),
		peers:   make(map[string]peer),
		clients: make(map[string]peer),
		client:  &http.Client{Timeout: 8 * time.Second},
		transfer: &http.Client{
			Timeout: 30 * time.Minute,
		},
		xfers:     make(map[string]*transferState),
		events:    make(map[string]map[chan clientEvent]bool),
		pending:   make(map[string][]clientEvent),
		downloads: make(map[string]browserDownload),
	}

	mux := http.NewServeMux()
	a.routes(mux)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           withCORS(logRequests(mux)),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go a.discoveryListener()
	go a.discoveryBroadcaster()
	go a.peerJanitor()
	go a.transferJanitor()
	go a.browserDownloadJanitor()
	go a.scanLoop()

	url := fmt.Sprintf("http://127.0.0.1:%d", *port)
	log.Printf("File Transfer is serving %s from %s", url, root)
	if !*noOpen {
		go func() {
			time.Sleep(700 * time.Millisecond)
			if err := openBrowser(url); err != nil {
				log.Printf("open browser: %v", err)
			}
		}()
	}
	log.Fatal(server.ListenAndServe())
}

func (a *app) routes(mux *http.ServeMux) {
	mux.HandleFunc("/", a.handleStatic)
	mux.HandleFunc("/favicon.ico", a.handleFavicon)
	mux.HandleFunc("/api/qr", a.handleQR)
	mux.HandleFunc("/api/self", a.handleSelf)
	mux.HandleFunc("/api/client", a.handleClient)
	mux.HandleFunc("/api/client/events", a.handleClientEvents)
	mux.HandleFunc("/api/client/send", a.handleClientSend)
	mux.HandleFunc("/api/client/download", a.handleClientDownload)
	mux.HandleFunc("/api/peers", a.handlePeers)
	mux.HandleFunc("/api/scan", a.handleScan)
	mux.HandleFunc("/api/files", a.handleFiles)
	mux.HandleFunc("/api/open", a.handleOpenFile)
	mux.HandleFunc("/api/save", a.handleSaveFile)
	mux.HandleFunc("/api/download", a.handleDownload)
	mux.HandleFunc("/api/upload", a.handleUpload)
	mux.HandleFunc("/api/mkdir", a.handleMkdir)
	mux.HandleFunc("/api/transfers", a.handleTransfers)
	mux.HandleFunc("/api/peer/", a.handlePeerProxy)
}

func (a *app) handleStatic(w http.ResponseWriter, r *http.Request) {
	requestPath := r.URL.Path
	if requestPath == "/" {
		requestPath = "/index.html"
	}
	assetPath := "static" + pathpkg.Clean(requestPath)
	if !strings.HasPrefix(assetPath, "static/") {
		http.NotFound(w, r)
		return
	}
	data, err := staticFiles.ReadFile(assetPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ext := pathpkg.Ext(assetPath); ext != "" {
		if ext == ".webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
		} else if ct := mime.TypeByExtension(ext); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
	}
	w.Write(data)
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Transfer-ID, X-Transfer-Name")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="14" fill="#101316"/><path d="M17 23h30v7H17zM17 34h30v7H17z" fill="#39d2a0"/><path d="M21 19h15l5 5H21z" fill="#4aa3ff"/></svg>`)
}

func (a *app) handleQR(w http.ResponseWriter, r *http.Request) {
	text := r.URL.Query().Get("text")
	if text == "" {
		text = a.baseURL
	}
	svg, err := makeQRSVG(text)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, svg)
}

func (a *app) handleSelf(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"id":      a.id,
		"name":    a.name,
		"ips":     localIPList(),
		"root":    a.root,
		"port":    a.port,
		"url":     a.baseURL,
		"os":      runtime.GOOS,
		"started": a.started,
	})
}

func (a *app) handleClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rawID := strings.TrimSpace(req.ID)
	if rawID == "" {
		http.Error(w, "client id is required", http.StatusBadRequest)
		return
	}
	clientID := normalizeBrowserClientID(rawID)
	host := remoteHost(r)
	if host == "" || isLocalHostAddress(host) {
		writeJSON(w, map[string]string{"status": "ignored"})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		req.Name = "Browser client"
	}
	req.Name = strings.ToUpper(strings.TrimSpace(req.Name))
	if len([]rune(req.Name)) > 4 {
		req.Name = string([]rune(req.Name)[:4])
	}
	a.peersMu.Lock()
	a.clients[clientID] = peer{
		ID:         clientID,
		Name:       req.Name,
		Host:       host,
		Port:       0,
		URL:        "",
		OS:         "browser",
		Kind:       "browser",
		CanReceive: false,
		LastSeen:   time.Now(),
	}
	a.peersMu.Unlock()
	writeJSON(w, map[string]string{"status": "ok", "id": clientID})
}

func (a *app) handleClientEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	clientID := normalizeBrowserClientID(r.URL.Query().Get("id"))
	if clientID == "browser-" {
		http.Error(w, "client id is required", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := make(chan clientEvent, 16)
	a.clientMu.Lock()
	if a.events[clientID] == nil {
		a.events[clientID] = make(map[chan clientEvent]bool)
	}
	a.events[clientID][ch] = true
	pending := append([]clientEvent(nil), a.pending[clientID]...)
	delete(a.pending, clientID)
	a.clientMu.Unlock()
	defer func() {
		a.clientMu.Lock()
		delete(a.events[clientID], ch)
		if len(a.events[clientID]) == 0 {
			delete(a.events, clientID)
		}
		a.clientMu.Unlock()
		close(ch)
	}()
	writeSSE := func(event clientEvent) bool {
		data, _ := json.Marshal(event)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	for _, event := range pending {
		if !writeSSE(event) {
			return
		}
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event := <-ch:
			if !writeSSE(event) {
				return
			}
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (a *app) handleClientSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	clientID := normalizeBrowserClientID(r.URL.Query().Get("id"))
	a.peersMu.RLock()
	_, ok := a.clients[clientID]
	a.peersMu.RUnlock()
	if !ok {
		http.Error(w, "browser client not found", http.StatusNotFound)
		return
	}
	if err := r.ParseMultipartForm(1 << 30); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	formFiles := r.MultipartForm.File["files"]
	sent := 0
	for _, header := range formFiles {
		src, err := header.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := loadRandomID()
		name := filepath.Base(header.Filename)
		tmp, err := os.CreateTemp("", "file-transfer-browser-*"+filepath.Ext(name))
		if err != nil {
			src.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		size, copyErr := io.Copy(tmp, src)
		closeErr := tmp.Close()
		src.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(tmp.Name())
			http.Error(w, "failed to stage browser download", http.StatusInternalServerError)
			return
		}
		a.clientMu.Lock()
		a.downloads[id] = browserDownload{
			ID:       id,
			ClientID: clientID,
			Name:     name,
			Path:     tmp.Name(),
			Size:     size,
			Created:  time.Now(),
		}
		a.clientMu.Unlock()
		event := clientEvent{
			Type: "download",
			ID:   id,
			Name: name,
			Size: size,
			URL:  "/api/client/download?id=" + url.QueryEscape(clientID) + "&transfer=" + url.QueryEscape(id),
		}
		a.sendClientEvent(clientID, event)
		sent++
	}
	writeJSON(w, map[string]any{"sent": sent})
}

func (a *app) handleClientDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	clientID := normalizeBrowserClientID(r.URL.Query().Get("id"))
	transferID := r.URL.Query().Get("transfer")
	a.clientMu.Lock()
	item, ok := a.downloads[transferID]
	if ok && item.ClientID == clientID {
		delete(a.downloads, transferID)
	}
	a.clientMu.Unlock()
	if !ok || item.ClientID != clientID {
		http.Error(w, "download not found", http.StatusNotFound)
		return
	}
	defer os.Remove(item.Path)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", item.Name))
	http.ServeFile(w, r, item.Path)
}

func (a *app) handlePeers(w http.ResponseWriter, r *http.Request) {
	a.peersMu.RLock()
	out := make([]peer, 0, len(a.peers)+len(a.clients))
	serverHosts := make(map[string]bool, len(a.peers))
	for _, p := range a.peers {
		p.CanReceive = true
		if p.Kind == "" {
			p.Kind = "server"
		}
		serverHosts[p.Host] = true
		out = append(out, p)
	}
	for _, p := range a.clients {
		if serverHosts[p.Host] {
			continue
		}
		out = append(out, p)
	}
	a.peersMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	writeJSON(w, out)
}

func (a *app) handleScan(w http.ResponseWriter, r *http.Request) {
	go a.scanNetwork()
	writeJSON(w, map[string]string{"status": "scanning"})
}

func (a *app) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel := r.URL.Query().Get("path")
	full, err := a.safePath(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if !info.IsDir() {
		http.Error(w, "path is not a directory", http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files := make([]fileEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		typ := "file"
		if info.IsDir() {
			typ = "dir"
		}
		child := filepath.ToSlash(filepath.Join(rel, entry.Name()))
		files = append(files, fileEntry{
			Name:     entry.Name(),
			Path:     cleanRel(child),
			Type:     typ,
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Type != files[j].Type {
			return files[i].Type == "dir"
		}
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	writeJSON(w, map[string]any{
		"path":    cleanRel(rel),
		"root":    a.root,
		"entries": files,
	})
}

func (a *app) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	full, err := a.safePath(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot download a folder", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(full)))
	http.ServeFile(w, r, full)
}

func (a *app) handleOpenFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	rel := r.URL.Query().Get("path")
	full, err := a.safePath(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot open a folder in the editor", http.StatusBadRequest)
		return
	}
	if info.Size() > maxEditBytes {
		http.Error(w, "file is too large for the text editor", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
		http.Error(w, "only UTF-8 text files can be edited", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{
		"path":     cleanRel(rel),
		"name":     filepath.Base(full),
		"content":  string(data),
		"size":     info.Size(),
		"modified": info.ModTime(),
	})
}

func (a *app) handleSaveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEditBytes+1024)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len([]byte(req.Content)) > maxEditBytes {
		http.Error(w, "file is too large for the text editor", http.StatusBadRequest)
		return
	}
	if !utf8.ValidString(req.Content) || strings.ContainsRune(req.Content, '\x00') {
		http.Error(w, "only UTF-8 text files can be saved", http.StatusBadRequest)
		return
	}
	full, err := a.safePath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if info.IsDir() {
		http.Error(w, "cannot save over a folder", http.StatusBadRequest)
		return
	}
	mode := info.Mode().Perm()
	if err := os.WriteFile(full, []byte(req.Content), mode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updated, _ := os.Stat(full)
	writeJSON(w, map[string]any{
		"path":     cleanRel(req.Path),
		"size":     updated.Size(),
		"modified": updated.ModTime(),
	})
}

func (a *app) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	destRel := r.URL.Query().Get("path")
	dest, err := a.safePath(destRel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(dest)
	if err != nil || !info.IsDir() {
		http.Error(w, "destination folder not found", http.StatusBadRequest)
		return
	}
	transferID := r.Header.Get("X-Transfer-ID")
	if transferID == "" {
		transferID = loadRandomID()
	}
	transferName := r.Header.Get("X-Transfer-Name")
	if transferName == "" {
		transferName = "Incoming files"
	}
	a.startTransfer(transferID, transferName, "inbound", r.ContentLength)
	r.Body = io.NopCloser(&progressReader{
		reader: r.Body,
		onRead: func(n int64) {
			a.addTransferBytes(transferID, n)
		},
	})
	if err := r.ParseMultipartForm(1 << 30); err != nil {
		a.finishTransfer(transferID, "failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	formFiles := r.MultipartForm.File["files"]
	saved := make([]fileEntry, 0, len(formFiles))
	for _, header := range formFiles {
		src, err := header.Open()
		if err != nil {
			a.finishTransfer(transferID, "failed")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		name := filepath.Base(header.Filename)
		target, err := uniqueTarget(dest, name)
		if err != nil {
			src.Close()
			a.finishTransfer(transferID, "failed")
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dst, err := os.Create(target)
		if err != nil {
			src.Close()
			a.finishTransfer(transferID, "failed")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil || closeErr != nil {
			a.finishTransfer(transferID, "failed")
			http.Error(w, "failed to save upload", http.StatusInternalServerError)
			return
		}
		info, _ := os.Stat(target)
		saved = append(saved, fileEntry{
			Name:     filepath.Base(target),
			Path:     cleanRel(filepath.ToSlash(filepath.Join(destRel, filepath.Base(target)))),
			Type:     "file",
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
	}
	a.finishTransfer(transferID, "complete")
	writeJSON(w, map[string]any{"saved": saved})
}

func (a *app) handleTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	a.xfersMu.RLock()
	out := make([]transferState, 0, len(a.xfers))
	for _, item := range a.xfers {
		out = append(out, *item)
	}
	a.xfersMu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Updated.After(out[j].Updated)
	})
	writeJSON(w, out)
}

func (a *app) handleMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		http.Error(w, "folder name is required", http.StatusBadRequest)
		return
	}
	parent, err := a.safePath(req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := a.safePath(filepath.ToSlash(filepath.Join(req.Path, filepath.Base(req.Name))))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(target, parent) {
		http.Error(w, "invalid folder name", http.StatusBadRequest)
		return
	}
	if err := os.Mkdir(target, 0755); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"path": cleanRel(filepath.ToSlash(filepath.Join(req.Path, filepath.Base(req.Name))))})
}

func (a *app) handlePeerProxy(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/peer/"), "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	peerID, action := parts[0], parts[1]
	p, ok := a.getPeer(peerID)
	if !ok {
		http.Error(w, "peer not found", http.StatusNotFound)
		return
	}
	switch action {
	case "files":
		a.proxyPeerFiles(w, r, p)
	case "open":
		a.proxyPeerOpen(w, r, p)
	case "save":
		a.proxyJSON(w, r, p.URL+"/api/save")
	case "download":
		a.proxyPeerDownload(w, r, p)
	case "mkdir":
		a.proxyJSON(w, r, p.URL+"/api/mkdir")
	case "upload":
		a.proxyUpload(w, r, p.URL+"/api/upload?path="+url.QueryEscape(r.URL.Query().Get("path")))
	default:
		http.NotFound(w, r)
	}
}

func (a *app) proxyPeerFiles(w http.ResponseWriter, r *http.Request, p peer) {
	target := p.URL + "/api/files?path=" + url.QueryEscape(r.URL.Query().Get("path"))
	resp, err := a.transfer.Get(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

func (a *app) proxyPeerDownload(w http.ResponseWriter, r *http.Request, p peer) {
	target := p.URL + "/api/download?path=" + url.QueryEscape(r.URL.Query().Get("path"))
	resp, err := a.transfer.Get(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

func (a *app) proxyPeerOpen(w http.ResponseWriter, r *http.Request, p peer) {
	target := p.URL + "/api/open?path=" + url.QueryEscape(r.URL.Query().Get("path"))
	resp, err := a.transfer.Get(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

func (a *app) proxyJSON(w http.ResponseWriter, r *http.Request, url string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	resp, err := a.transfer.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

func (a *app) proxyUpload(w http.ResponseWriter, r *http.Request, url string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	req.Header.Set("X-Transfer-ID", r.Header.Get("X-Transfer-ID"))
	req.Header.Set("X-Transfer-Name", r.Header.Get("X-Transfer-Name"))
	req.ContentLength = r.ContentLength
	resp, err := a.transfer.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

func (a *app) safePath(rel string) (string, error) {
	rel = cleanRel(rel)
	full := filepath.Join(a.root, filepath.FromSlash(rel))
	fullClean, err := filepath.Abs(full)
	if err != nil {
		return "", err
	}
	rootClean, err := filepath.Abs(a.root)
	if err != nil {
		return "", err
	}
	if fullClean != rootClean && !strings.HasPrefix(fullClean, rootClean+string(os.PathSeparator)) {
		return "", errors.New("path escapes shared folder")
	}
	return fullClean, nil
}

func cleanRel(rel string) string {
	rel = strings.TrimSpace(rel)
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." {
		return "."
	}
	return clean
}

func uniqueTarget(dir, name string) (string, error) {
	if name == "." || name == string(os.PathSeparator) || strings.TrimSpace(name) == "" {
		return "", errors.New("invalid file name")
	}
	base := filepath.Base(name)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	target := filepath.Join(dir, base)
	for i := 1; ; i++ {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			return target, nil
		}
		target = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func (a *app) discoveryBroadcaster() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		a.broadcastOnce()
		<-ticker.C
	}
}

func (a *app) broadcastOnce() {
	msg, _ := json.Marshal(discoveryMessage{ID: a.id, Name: a.name, Port: a.port, OS: runtime.GOOS})
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4bcast, Port: discoveryPort})
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(msg)
}

func (a *app) discoveryListener() {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: discoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		log.Printf("discovery listener: %v", err)
		return
	}
	defer conn.Close()
	buf := make([]byte, 2048)
	for {
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var msg discoveryMessage
		if err := json.Unmarshal(buf[:n], &msg); err != nil || msg.ID == "" || msg.ID == a.id {
			continue
		}
		a.rememberPeer(peer{
			ID:         msg.ID,
			Name:       msg.Name,
			Host:       remote.IP.String(),
			Port:       msg.Port,
			URL:        fmt.Sprintf("http://%s:%d", remote.IP.String(), msg.Port),
			OS:         msg.OS,
			Kind:       "server",
			CanReceive: true,
			LastSeen:   time.Now(),
		})
	}
}

func (a *app) scanLoop() {
	time.Sleep(600 * time.Millisecond)
	a.scanNetwork()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		a.scanNetwork()
	}
}

func (a *app) scanNetwork() {
	a.scanMu.Lock()
	if time.Since(a.scanTime) < 4*time.Second {
		a.scanMu.Unlock()
		return
	}
	a.scanTime = time.Now()
	a.scanMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	ips := candidateLANIPs()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for _, ip := range ips {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/api/self", ip, a.port), nil)
			resp, err := a.client.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return
			}
			var data struct {
				ID   string `json:"id"`
				Name string `json:"name"`
				Port int    `json:"port"`
				OS   string `json:"os"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil || data.ID == "" || data.ID == a.id {
				return
			}
			port := data.Port
			if port == 0 {
				port = a.port
			}
			a.rememberPeer(peer{
				ID:         data.ID,
				Name:       data.Name,
				Host:       ip,
				Port:       port,
				URL:        fmt.Sprintf("http://%s:%d", ip, port),
				OS:         data.OS,
				Kind:       "server",
				CanReceive: true,
				LastSeen:   time.Now(),
			})
		}()
	}
	wg.Wait()
}

func candidateLANIPs() []string {
	seen := map[string]bool{}
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, ipnet, ok := parseCIDRAddr(addr)
			if !ok || ip.To4() == nil {
				continue
			}
			mask, bits := ipnet.Mask.Size()
			if bits != 32 || mask < 24 {
				continue
			}
			network := ip.Mask(ipnet.Mask).To4()
			if network == nil {
				continue
			}
			limit := 1 << uint(bits-mask)
			if limit > 256 {
				limit = 256
			}
			for i := 1; i < limit-1; i++ {
				candidate := net.IPv4(network[0], network[1], network[2], byte(i)).String()
				if candidate == ip.String() || seen[candidate] {
					continue
				}
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	return out
}

func parseCIDRAddr(addr net.Addr) (net.IP, *net.IPNet, bool) {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP, v, true
	default:
		ip, ipnet, err := net.ParseCIDR(v.String())
		return ip, ipnet, err == nil
	}
}

func (a *app) rememberPeer(p peer) {
	if p.ID == "" || p.ID == a.id {
		return
	}
	if p.Name == "" {
		p.Name = p.Host
	}
	p.Kind = "server"
	p.CanReceive = true
	a.peersMu.Lock()
	a.peers[p.ID] = p
	a.peersMu.Unlock()
}

func (a *app) getPeer(id string) (peer, bool) {
	a.peersMu.RLock()
	p, ok := a.peers[id]
	a.peersMu.RUnlock()
	return p, ok
}

func (a *app) peerJanitor() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-peerTTL)
		a.peersMu.Lock()
		for id, p := range a.peers {
			if p.LastSeen.Before(cutoff) {
				delete(a.peers, id)
			}
		}
		for id, p := range a.clients {
			if p.LastSeen.Before(cutoff) {
				delete(a.clients, id)
			}
		}
		a.peersMu.Unlock()
	}
}

func (a *app) sendClientEvent(clientID string, event clientEvent) {
	a.clientMu.Lock()
	listeners := a.events[clientID]
	if len(listeners) == 0 {
		a.pending[clientID] = append(a.pending[clientID], event)
		a.clientMu.Unlock()
		return
	}
	for ch := range listeners {
		select {
		case ch <- event:
		default:
			a.pending[clientID] = append(a.pending[clientID], event)
		}
	}
	a.clientMu.Unlock()
}

func (a *app) browserDownloadJanitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-15 * time.Minute)
		var paths []string
		a.clientMu.Lock()
		for id, item := range a.downloads {
			if item.Created.Before(cutoff) {
				paths = append(paths, item.Path)
				delete(a.downloads, id)
			}
		}
		for id, pending := range a.pending {
			kept := pending[:0]
			for _, event := range pending {
				if item, ok := a.downloads[event.ID]; ok && item.Created.After(cutoff) {
					kept = append(kept, event)
				}
			}
			if len(kept) == 0 {
				delete(a.pending, id)
			} else {
				a.pending[id] = kept
			}
		}
		a.clientMu.Unlock()
		for _, path := range paths {
			os.Remove(path)
		}
	}
}

func normalizeBrowserClientID(id string) string {
	id = strings.TrimSpace(id)
	if strings.HasPrefix(id, "browser-") {
		return id
	}
	return "browser-" + id
}

func makeQRSVG(text string) (string, error) {
	data := []byte(text)
	if len(data) > 53 {
		return "", errors.New("QR text is too long")
	}
	const version = 3
	const size = 29
	const dataWords = 55
	const ecWords = 15
	mod := make([][]int, size)
	reserved := make([][]bool, size)
	for y := range mod {
		mod[y] = make([]int, size)
		reserved[y] = make([]bool, size)
		for x := range mod[y] {
			mod[y][x] = -1
		}
	}
	set := func(x, y int, dark bool) {
		if x < 0 || y < 0 || x >= size || y >= size {
			return
		}
		if dark {
			mod[y][x] = 1
		} else {
			mod[y][x] = 0
		}
		reserved[y][x] = true
	}
	drawFinder := func(x, y int) {
		for dy := -1; dy <= 7; dy++ {
			for dx := -1; dx <= 7; dx++ {
				xx, yy := x+dx, y+dy
				if xx < 0 || yy < 0 || xx >= size || yy >= size {
					continue
				}
				dark := dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 &&
					(dx == 0 || dx == 6 || dy == 0 || dy == 6 || (dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4))
				set(xx, yy, dark)
			}
		}
	}
	drawFinder(0, 0)
	drawFinder(size-7, 0)
	drawFinder(0, size-7)
	for i := 8; i < size-8; i++ {
		set(i, 6, i%2 == 0)
		set(6, i, i%2 == 0)
	}
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			d := max(abs(dx), abs(dy))
			set(22+dx, 22+dy, d != 1)
		}
	}
	set(8, 4*version+9, true)
	for i := 0; i <= 8; i++ {
		if i != 6 {
			reserved[8][i] = true
			reserved[i][8] = true
		}
	}
	for i := 0; i < 8; i++ {
		reserved[8][size-1-i] = true
		reserved[size-1-i][8] = true
	}

	bits := make([]bool, 0, 8*dataWords)
	appendBits := func(value, count int) {
		for i := count - 1; i >= 0; i-- {
			bits = append(bits, ((value>>i)&1) != 0)
		}
	}
	appendBits(4, 4)
	appendBits(len(data), 8)
	for _, b := range data {
		appendBits(int(b), 8)
	}
	for i := 0; i < 4 && len(bits) < dataWords*8; i++ {
		bits = append(bits, false)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	pads := []byte{0xec, 0x11}
	for i := 0; len(bits) < dataWords*8; i++ {
		appendBits(int(pads[i%2]), 8)
	}
	words := make([]byte, dataWords)
	for i := range words {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i*8+j] {
				b |= 1 << uint(7-j)
			}
		}
		words[i] = b
	}
	codewords := append(words, reedSolomon(words, ecWords)...)
	stream := make([]bool, 0, len(codewords)*8+7)
	for _, b := range codewords {
		for i := 7; i >= 0; i-- {
			stream = append(stream, ((b>>uint(i))&1) != 0)
		}
	}
	for len(stream) < len(codewords)*8+7 {
		stream = append(stream, false)
	}
	bitIndex := 0
	up := true
	for x := size - 1; x >= 1; x -= 2 {
		if x == 6 {
			x--
		}
		for i := 0; i < size; i++ {
			y := i
			if up {
				y = size - 1 - i
			}
			for dx := 0; dx < 2; dx++ {
				xx := x - dx
				if reserved[y][xx] {
					continue
				}
				dark := false
				if bitIndex < len(stream) {
					dark = stream[bitIndex]
					bitIndex++
				}
				if (xx+y)%2 == 0 {
					dark = !dark
				}
				if dark {
					mod[y][xx] = 1
				} else {
					mod[y][xx] = 0
				}
			}
		}
		up = !up
	}
	format := qrFormatBits(1, 0)
	for i := 0; i <= 5; i++ {
		set(8, i, ((format>>i)&1) != 0)
	}
	set(8, 7, ((format>>6)&1) != 0)
	set(8, 8, ((format>>7)&1) != 0)
	set(7, 8, ((format>>8)&1) != 0)
	for i := 9; i < 15; i++ {
		set(14-i, 8, ((format>>i)&1) != 0)
	}
	for i := 0; i < 8; i++ {
		set(size-1-i, 8, ((format>>i)&1) != 0)
	}
	for i := 8; i < 15; i++ {
		set(8, size-15+i, ((format>>i)&1) != 0)
	}
	var b strings.Builder
	scale := 6
	quiet := 4
	total := (size + quiet*2) * scale
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">`, total, total, total, total)
	b.WriteString(`<rect width="100%" height="100%" fill="#fff"/>`)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if mod[y][x] == 1 {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" fill="#000"/>`, (x+quiet)*scale, (y+quiet)*scale, scale, scale)
			}
		}
	}
	b.WriteString(`</svg>`)
	return b.String(), nil
}

func qrFormatBits(ecl, mask int) int {
	data := (ecl << 3) | mask
	v := data << 10
	for i := 14; i >= 10; i-- {
		if ((v >> i) & 1) != 0 {
			v ^= 0x537 << uint(i-10)
		}
	}
	return ((data << 10) | v) ^ 0x5412
}

func reedSolomon(data []byte, ecWords int) []byte {
	gen := []byte{1}
	for i := 0; i < ecWords; i++ {
		next := make([]byte, len(gen)+1)
		for j, c := range gen {
			next[j] ^= gfMul(c, 1)
			next[j+1] ^= gfMul(c, gfPow(2, i))
		}
		gen = next
	}
	ec := make([]byte, ecWords)
	for _, d := range data {
		factor := d ^ ec[0]
		copy(ec, ec[1:])
		ec[ecWords-1] = 0
		for i := 0; i < ecWords; i++ {
			ec[i] ^= gfMul(gen[i+1], factor)
		}
	}
	return ec
}

func gfPow(x byte, power int) byte {
	out := byte(1)
	for i := 0; i < power; i++ {
		out = gfMul(out, x)
	}
	return out
}

func gfMul(x, y byte) byte {
	var z int
	a, b := int(x), int(y)
	for b > 0 {
		if b&1 != 0 {
			z ^= a
		}
		a <<= 1
		if a&0x100 != 0 {
			a ^= 0x11d
		}
		b >>= 1
	}
	return byte(z)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func isLocalHostAddress(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return host == "localhost"
	}
	if ip.IsLoopback() {
		return true
	}
	for _, local := range localIPList() {
		if host == local {
			return true
		}
	}
	return false
}

func (a *app) startTransfer(id, name, direction string, total int64) {
	now := time.Now()
	a.xfersMu.Lock()
	a.xfers[id] = &transferState{
		ID:        id,
		Name:      name,
		Direction: direction,
		Total:     total,
		Status:    "active",
		Started:   now,
		Updated:   now,
	}
	a.xfersMu.Unlock()
}

func (a *app) addTransferBytes(id string, n int64) {
	a.xfersMu.Lock()
	if item, ok := a.xfers[id]; ok {
		item.Bytes += n
		item.Updated = time.Now()
	}
	a.xfersMu.Unlock()
}

func (a *app) finishTransfer(id, status string) {
	a.xfersMu.Lock()
	if item, ok := a.xfers[id]; ok {
		if item.Total > 0 && item.Bytes > item.Total {
			item.Bytes = item.Total
		}
		item.Status = status
		item.Updated = time.Now()
	}
	a.xfersMu.Unlock()
}

func (a *app) transferJanitor() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-45 * time.Second)
		a.xfersMu.Lock()
		for id, item := range a.xfers {
			if item.Status != "active" && item.Updated.Before(cutoff) {
				delete(a.xfers, id)
			}
		}
		a.xfersMu.Unlock()
	}
}

func localAdvertiseIP() string {
	ips := localIPList()
	if len(ips) > 0 {
		return ips[0]
	}
	return "127.0.0.1"
}

func localIPList() []string {
	seen := map[string]bool{}
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ip = ip4
			}
			text := ip.String()
			if text == "" || strings.HasPrefix(text, "127.") || text == "::1" || seen[text] {
				continue
			}
			seen[text] = true
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}

func loadOrCreateID(root string) string {
	path := filepath.Join(root, ".file-transfer-id")
	if b, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(b))
		if id != "" {
			return id
		}
	}
	id := loadRandomID()
	_ = os.WriteFile(path, []byte(id), 0600)
	return id
}

func loadRandomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func methodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
	})
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}
