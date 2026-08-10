package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Config holds the application configuration
type Config struct {
	HugoSiteDir    string
	HugoServerCmd  string
	ServerPort     int
	AutosaveDelay  time.Duration
	HugoServerPort int
}

var (
	config Config
	mu     sync.Mutex // Mutex for file operations
)

// previewState tracks the on-demand Hugo preview server's lifecycle.
var previewState = struct {
	mu            sync.Mutex
	cmd           *exec.Cmd
	running       bool
	lastHeartbeat time.Time
}{}

func main() {
	// Parse command line flags
	flag.StringVar(&config.HugoSiteDir, "site", "", "Path to Hugo site directory")
	flag.StringVar(&config.HugoServerCmd, "hugo-cmd", "hugo server -D", "Command to run Hugo server")
	flag.IntVar(&config.ServerPort, "port", 8080, "Port for the editor server")
	flag.DurationVar(&config.AutosaveDelay, "autosave", 2*time.Second, "Delay for autosaving")
	flag.IntVar(&config.HugoServerPort, "hugo-port", 1313, "Port for the Hugo server")
	flag.Parse()

	// Validate configuration
	if config.HugoSiteDir == "" {
		log.Fatal("Hugo site directory must be specified with -site flag")
	}

	// Ensure the Hugo site directory exists
	if _, err := os.Stat(config.HugoSiteDir); os.IsNotExist(err) {
		log.Fatalf("Hugo site directory does not exist: %s", config.HugoSiteDir)
	}

	// Watch for idle previews and stop the Hugo server once nobody is watching
	go watchPreviewIdle()

	// Set up HTTP routes
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/edit/", handleEdit)
	http.HandleFunc("/save", handleSave)
	http.HandleFunc("/new", handleNew)
	http.HandleFunc("/api/preview-heartbeat", handlePreviewHeartbeat)
	http.Handle("/preview/", newPreviewProxy())

	// Serve static files
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	server := &http.Server{
		Addr:         fmt.Sprintf(":%d", config.ServerPort),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Set up shutdown handling
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan

		log.Println("Shutting down server...")
		err := stopHugoServer()
		if err != nil {
			log.Println("Error shutting down hugo server", err.Error())
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err = server.Shutdown(ctx)
		if err != nil {
			log.Println("Error shutting down web server", err.Error())
		}
	}()

	// Start the server
	log.Printf("Starting editor server at http://localhost%s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

// Post represents a Hugo markdown post
type Post struct {
	Path    string
	Content string
	Title   string
	Date    string
	IsNew   bool
}

// ensureHugoServerRunning starts the Hugo server on demand if it isn't
// already running, then waits for it to start accepting connections.
func ensureHugoServerRunning() error {
	previewState.mu.Lock()
	if previewState.running {
		previewState.lastHeartbeat = time.Now()
		previewState.mu.Unlock()
		return nil
	}
	startHugoServerLocked()
	previewState.lastHeartbeat = time.Now()
	previewState.mu.Unlock()

	return waitForHugoServerReady(5 * time.Second)
}

// startHugoServerLocked starts the Hugo server in development mode.
// Callers must hold previewState.mu.
func startHugoServerLocked() {
	// Split the command into parts
	parts := strings.Fields(config.HugoServerCmd)
	if len(parts) == 0 {
		log.Fatal("Invalid Hugo server command")
	}

	// Add the hugo port argument
	parts = append(parts, "--port", fmt.Sprintf("%d", config.HugoServerPort))

	// Add the site directory
	parts = append(parts, "--source", config.HugoSiteDir)

	// The preview is served through hugo-editor's own /preview/ proxy, so
	// Hugo must generate asset and livereload links (and mount its own
	// routes) under that path/port instead of its own raw address. The
	// literal host baked in here gets rewritten per-request by the proxy
	// (see newPreviewProxy) to whatever host the browser actually used,
	// since editors may be reached via a different IP/hostname than
	// "localhost".
	parts = append(parts,
		"--baseURL", hugoInternalBaseURL(),
		"--appendPort=false",
		"--liveReloadPort", fmt.Sprintf("%d", config.ServerPort),
	)

	// Create the command
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = config.HugoSiteDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	log.Printf("Starting Hugo server with command: %s %s", cmd.Path, strings.Join(cmd.Args, " "))
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start Hugo server: %v", err)
	}

	previewState.cmd = cmd
	previewState.running = true
}

// waitForHugoServerReady polls the configured Hugo port until it accepts
// TCP connections, or the timeout elapses.
func waitForHugoServerReady(timeout time.Duration) error {
	addr := fmt.Sprintf("localhost:%d", config.HugoServerPort)
	deadline := time.Now().Add(timeout)

	for {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			conn.Close()
			log.Printf("Hugo server running at http://%s", addr)
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for Hugo server at %s: %w", addr, err)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

// stopHugoServer stops the Hugo server if it's running
func stopHugoServer() error {
	previewState.mu.Lock()
	defer previewState.mu.Unlock()

	if previewState.cmd == nil || previewState.cmd.Process == nil {
		previewState.running = false
		return nil
	}

	log.Println("Stopping Hugo server...")
	cmd := previewState.cmd

	// Send interrupt signal to allow graceful shutdown
	var err error
	if sigErr := cmd.Process.Signal(os.Interrupt); sigErr != nil {
		// If interrupt fails, try to kill the process
		err = cmd.Process.Kill()
	} else {
		// Wait for the process to exit
		err = cmd.Wait()
	}

	previewState.cmd = nil
	previewState.running = false
	return err
}

// watchPreviewIdle stops the Hugo server once the editor page's heartbeat
// pings stop arriving (tab closed, navigated away, or crashed).
func watchPreviewIdle() {
	for range time.Tick(5 * time.Second) {
		previewState.mu.Lock()
		idle := previewState.running && time.Since(previewState.lastHeartbeat) > 20*time.Second
		previewState.mu.Unlock()

		if idle {
			if err := stopHugoServer(); err != nil {
				log.Printf("Error stopping idle Hugo server: %v", err)
			}
		}
	}
}

// hugoInternalBaseURL is the baseURL Hugo is told to render with. It always
// names "localhost" since it's only used internally to get Hugo to mount its
// routes and generate links under /preview/ - the proxy below rewrites the
// literal host in responses to match whatever host the browser actually used.
func hugoInternalBaseURL() string {
	return fmt.Sprintf("http://localhost:%d/preview/", config.ServerPort)
}

type originalRequestKey struct{}

// newPreviewProxy returns a reverse proxy to the on-demand Hugo server,
// starting it on demand before the first request is forwarded, and
// rewriting any baked-in hugoInternalBaseURL references in text responses
// to the host the browser actually connected to (e.g. a Tailscale IP or
// LAN hostname instead of "localhost").
func newPreviewProxy() http.Handler {
	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("localhost:%d", config.HugoServerPort)}
	proxy := httputil.NewSingleHostReverseProxy(target)

	proxy.ModifyResponse = func(resp *http.Response) error {
		ct := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "xml") && !strings.Contains(ct, "javascript") && !strings.Contains(ct, "json") {
			return nil
		}

		origReq, _ := resp.Request.Context().Value(originalRequestKey{}).(*http.Request)
		if origReq == nil {
			return nil
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}

		scheme := "http"
		if origReq.TLS != nil || origReq.Header.Get("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		externalBaseURL := fmt.Sprintf("%s://%s/preview/", scheme, origReq.Host)

		rewritten := bytes.ReplaceAll(body, []byte(hugoInternalBaseURL()), []byte(externalBaseURL))
		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := ensureHugoServerRunning(); err != nil {
			http.Error(w, fmt.Sprintf("Hugo server did not start in time: %v", err), http.StatusServiceUnavailable)
			return
		}
		ctx := context.WithValue(r.Context(), originalRequestKey{}, r)
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// handlePreviewHeartbeat is pinged periodically by the editor page while a
// preview panel is open, keeping the Hugo server alive.
func handlePreviewHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := ensureHugoServerRunning(); err != nil {
		http.Error(w, fmt.Sprintf("Hugo server did not start in time: %v", err), http.StatusServiceUnavailable)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// findMarkdownFiles finds all markdown files in the content directory
func findMarkdownFiles() ([]Post, error) {
	var posts []Post
	contentDir := filepath.Join(config.HugoSiteDir, "content", "blog")

	err := filepath.Walk(contentDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Only process markdown files
		if !strings.HasSuffix(strings.ToLower(info.Name()), ".md") {
			return nil
		}

		// Get relative path from content directory
		relPath, err := filepath.Rel(contentDir, path)
		if err != nil {
			return err
		}

		// Read file content to extract title and date
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		// Extract title and date from front matter
		title := ""
		date := ""
		lines := strings.Split(string(content), "\n")
		inFrontMatter := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "---" {
				if !inFrontMatter {
					inFrontMatter = true
				} else {
					break
				}
				continue
			}
			if inFrontMatter {
				if strings.HasPrefix(line, "title:") {
					title = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
					// Remove quotes if present
					title = strings.Trim(title, "\"'")
				} else if strings.HasPrefix(line, "date:") {
					date = strings.TrimSpace(strings.TrimPrefix(line, "date:"))
				}
			}
		}

		// If no title found, use filename
		if title == "" {
			title = strings.TrimSuffix(info.Name(), filepath.Ext(info.Name()))
		}

		// If no date found, use file modification time
		if date == "" {
			date = info.ModTime().Format("2006-01-02")
		}

		posts = append(posts, Post{
			Path:  relPath,
			Title: title,
			Date:  date,
		})

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Sort posts by date (newest first)
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date > posts[j].Date
	})

	return posts, nil
}

// prevents directory traversal
func validatePath(path string) (string, error) {
	// Clean the path and check for directory traversal
	cleanPath := filepath.Clean(path)
	if strings.Contains(cleanPath, "..") {
		return "", fmt.Errorf("invalid path: directory traversal not allowed")
	}
	if filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("invalid path: absolute paths not allowed")
	}
	return cleanPath, nil
}

// getPost retrieves a specific post by its path
func getPost(path string) (_ Post, err error) {
	if path, err = validatePath(path); err != nil {
		return Post{}, err
	}

	contentDir := filepath.Join(config.HugoSiteDir, "content", "blog")
	fullPath := filepath.Join(contentDir, path)

	// Check if file exists
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		return Post{}, err
	}

	// Read file content
	content, err := os.ReadFile(fullPath)
	if err != nil {
		return Post{}, err
	}

	// Extract title and date from front matter
	title := ""
	date := ""
	lines := strings.Split(string(content), "\n")
	inFrontMatter := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "---" {
			if !inFrontMatter {
				inFrontMatter = true
			} else {
				break
			}
			continue
		}
		if inFrontMatter {
			if strings.HasPrefix(line, "title:") {
				title = strings.TrimSpace(strings.TrimPrefix(line, "title:"))
				// Remove quotes if present
				title = strings.Trim(title, "\"'")
			} else if strings.HasPrefix(line, "date:") {
				date = strings.TrimSpace(strings.TrimPrefix(line, "date:"))
			}
		}
	}

	// If no title found, use filename
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(filepath.Base(path)))
	}

	// If no date found, use file modification time
	if date == "" {
		date = fileInfo.ModTime().Format("2006-01-02")
	}

	return Post{
		Path:    path,
		Content: string(content),
		Title:   title,
		Date:    date,
	}, nil
}

// savePost saves the content of a post
func savePost(path, content string) (err error) {
	if path, err = validatePath(path); err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()

	contentDir := filepath.Join(config.HugoSiteDir, "content", "blog")
	fullPath := filepath.Join(contentDir, path)

	// Ensure the directory exists
	dir := filepath.Dir(fullPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Write the content
	return os.WriteFile(fullPath, []byte(content), 0o644)
}

// Create a template function map
var funcMap = template.FuncMap{
	"trimSuffix": strings.TrimSuffix,
}

// handleIndex displays the home page with a list of posts
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// Find all markdown files
	posts, err := findMarkdownFiles()
	if err != nil {
		http.Error(w, fmt.Sprintf("Error finding posts: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse template with function map
	tmpl, err := template.New("base.html").Funcs(funcMap).ParseFiles("templates/base.html", "templates/index.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
		return
	}

	// Render template
	data := struct {
		Title string
		Posts []Post
	}{
		Title: "Home",
		Posts: posts,
	}

	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, fmt.Sprintf("Error rendering template: %v", err), http.StatusInternalServerError)
	}
}

// handleEdit displays the editor for a specific post
func handleEdit(w http.ResponseWriter, r *http.Request) {
	// Extract path from URL
	path := strings.TrimPrefix(r.URL.Path, "/edit/")
	if path == "" {
		http.Error(w, "No post specified", http.StatusBadRequest)
		return
	}

	// Get the post
	post, err := getPost(path)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error getting post: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse template with function map
	tmpl, err := template.New("base.html").Funcs(funcMap).ParseFiles("templates/base.html", "templates/editor.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
		return
	}

	// Render template
	data := struct {
		Title      string
		Path       string
		Content    string
		IsNew      bool
		PreviewURL string
	}{
		Title:      "Edit - " + post.Title,
		Path:       post.Path,
		Content:    post.Content,
		IsNew:      false,
		PreviewURL: "/preview/blog/" + strings.TrimSuffix(post.Path, ".md"),
	}

	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, fmt.Sprintf("Error rendering template: %v", err), http.StatusInternalServerError)
	}
}

// handleSave saves the content of a post
func handleSave(w http.ResponseWriter, r *http.Request) {
	// Only accept POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form
	if err := r.ParseMultipartForm(30 * 1024); err != nil {
		http.Error(w, fmt.Sprintf("Error parsing form: %v", err), http.StatusBadRequest)
		return
	}

	// Get form values. We use multi-part in the editor
	path := r.MultipartForm.Value["path"][0]
	content := r.MultipartForm.Value["content"][0]
	fmt.Println(r.Form)

	if content == "" {
		http.Error(w, "Missing required fields", http.StatusBadRequest)
		return
	}

	// Extract title and date from front matter
	title := "new-post"
	var year, month string
	currentTime := time.Now()
	year = currentTime.Format("2006")
	month = currentTime.Format("01")

	lines := strings.Split(content, "\n")
	inFrontMatter := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "---" {
			if !inFrontMatter {
				inFrontMatter = true
			} else {
				break
			}
			continue
		}
		if inFrontMatter && strings.HasPrefix(line, "title:") {
			titleValue := strings.TrimSpace(strings.TrimPrefix(line, "title:"))
			// Remove quotes if present
			titleValue = strings.Trim(titleValue, "\"'")
			if titleValue != "" {
				title = titleValue
			}
		}

		if inFrontMatter && strings.HasPrefix(line, "date:") {
			// Try and parse the date from the front matter, get the year and month.
			dateValue := strings.TrimSpace(strings.TrimPrefix(line, "date:"))
			// Remove quotes if present
			dateValue = strings.Trim(dateValue, "\"'")

			// Try different date formats commonly used in Hugo
			var parsedTime time.Time
			var err error

			// Try RFC3339 format (2006-01-02T15:04:05Z07:00)
			parsedTime, err = time.Parse(time.RFC3339, dateValue)
			if err != nil {
				// Try simple date format (2006-01-02)
				parsedTime, err = time.Parse("2006-01-02", dateValue)
				if err != nil {
					// Try another common format (2006-01-02 15:04:05)
					parsedTime, err = time.Parse("2006-01-02 15:04:05", dateValue)
				}
			}

			if err == nil {
				// Get year and month for directory structure (e.g., 2023/05/)
				year = parsedTime.Format("2006")
				month = parsedTime.Format("01")
			}
		}
	}

	// Generate slug from title
	slug := strings.ToLower(title)
	slug = regexp.MustCompile(`[^\w\s-]`).ReplaceAllString(slug, "")
	slug = regexp.MustCompile(`\s+`).ReplaceAllString(slug, "-")

	// Format the filename with year and month: YYYY_MM_title.md
	newFilename := fmt.Sprintf("%s_%s_%s.md", year, month, slug)

	// If path doesn't exist, create it based on the title and date
	if path == "" {
		// Ensure slug is unique
		contentDir := filepath.Join(config.HugoSiteDir, "content", "blog")
		fullPath := filepath.Join(contentDir, newFilename)

		// If file exists, add a timestamp to make it unique
		if _, err := os.Stat(fullPath); err == nil {
			timestamp := time.Now().Format("20060102-150405")
			slug = slug + "-" + timestamp
			newFilename = fmt.Sprintf("%s_%s_%s.md", year, month, slug)
		}

		path = newFilename
	} else {
		fmt.Println("path", newFilename, "already set")
		// If the path is out of date, update the path and rename the underlying file
		oldFullPath := filepath.Join(config.HugoSiteDir, "content", "blog", path)
		newFullPath := filepath.Join(config.HugoSiteDir, "content", "blog", newFilename)

		// Only rename if the new path is different and doesn't already exist
		if path != newFilename {
			if _, err := os.Stat(newFullPath); os.IsNotExist(err) {
				// Ensure directory exists
				if err := os.MkdirAll(filepath.Dir(newFullPath), 0o755); err == nil {
					// Rename file
					if err := os.Rename(oldFullPath, newFullPath); err == nil {
						// Update path to new filename
						path = newFilename
					}
				}
			} else {
				// This shouldn't happen. Return an error
				http.Error(w, "Duplicated path name. Can't save!", http.StatusBadRequest)
			}
		}
	}

	// Save the post
	err := savePost(path, content)
	if err != nil {
		// Return JSON response with error
		w.Header().Set("Content-Type", "application/json")
		if encodeErr := json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   err.Error(),
		}); encodeErr != nil {
			log.Printf("Error encoding JSON response: %v", encodeErr)
		}
		return
	}

	// Return success JSON response
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"path":    path,
	})
}

// handleNew creates a new post
func handleNew(w http.ResponseWriter, r *http.Request) {
	// Handle GET request (show form)
	if r.Method == http.MethodGet {
		// Parse template with function map
		tmpl, err := template.New("base.html").Funcs(funcMap).ParseFiles("templates/base.html", "templates/editor.html")
		if err != nil {
			http.Error(w, fmt.Sprintf("Error parsing template: %v", err), http.StatusInternalServerError)
			return
		}

		// Generate a default empty post with front matter
		now := time.Now().Format("2006-01-02T15:04:05-07:00")
		defaultContent := fmt.Sprintf(`---
title: "New Post"
date: %s
draft: true
---

Write your content here...
`, now)

		// Render template
		data := struct {
			Title      string
			Path       string
			Content    string
			IsNew      bool
			PreviewURL string
		}{
			Title:      "New Post",
			Path:       "",
			Content:    defaultContent,
			IsNew:      true,
			PreviewURL: "",
		}

		if err := tmpl.Execute(w, data); err != nil {
			http.Error(w, fmt.Sprintf("Error rendering template: %v", err), http.StatusInternalServerError)
		}
		return
	}

	// Method not allowed
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}
