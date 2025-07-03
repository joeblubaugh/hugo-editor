package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config holds the application configuration
type Config struct {
	HugoSiteDir    string
	HugoServerCmd  string
	PublishCmd     string
	ServerPort     int
	AutosaveDelay  time.Duration
	HugoServerPort int
}

var (
	config     Config
	hugoServer *exec.Cmd
	mu         sync.Mutex // Mutex for file operations
)

func main() {
	// Parse command line flags
	flag.StringVar(&config.HugoSiteDir, "site", "", "Path to Hugo site directory")
	flag.StringVar(&config.HugoServerCmd, "hugo-cmd", "hugo server -D", "Command to run Hugo server")
	flag.StringVar(&config.PublishCmd, "publish-cmd", "hugo", "Command to build and publish the site")
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

	// Start the Hugo server
	startHugoServer()

	// Set up HTTP routes
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/edit/", handleEdit)
	http.HandleFunc("/save", handleSave)
	http.HandleFunc("/publish", handlePublish)
	http.HandleFunc("/new", handleNew)

	// Serve static files
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	// Start the server
	serverAddr := fmt.Sprintf(":%d", config.ServerPort)
	log.Printf("Starting editor server at http://localhost%s", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}

// Post represents a Hugo markdown post
type Post struct {
	Path    string
	Content string
	Title   string
	Date    string
	IsNew   bool
}

// startHugoServer starts the Hugo server in development mode
func startHugoServer() {
	// Split the command into parts
	parts := strings.Fields(config.HugoServerCmd)
	if len(parts) == 0 {
		log.Fatal("Invalid Hugo server command")
	}

	// Create the command
	hugoServer = exec.Command(parts[0], parts[1:]...)
	hugoServer.Dir = config.HugoSiteDir
	hugoServer.Stdout = os.Stdout
	hugoServer.Stderr = os.Stderr

	// Start the server
	log.Printf("Starting Hugo server with command: %s", config.HugoServerCmd)
	if err := hugoServer.Start(); err != nil {
		log.Fatalf("Failed to start Hugo server: %v", err)
	}

	// Set up cleanup on program exit
	go func() {
		<-time.After(2 * time.Second)
		log.Printf("Hugo server running at http://localhost:%d", config.HugoServerPort)
	}()
}

// stopHugoServer stops the Hugo server if it's running
func stopHugoServer() error {
	if hugoServer == nil || hugoServer.Process == nil {
		return nil
	}

	log.Println("Stopping Hugo server...")

	// Send interrupt signal to allow graceful shutdown
	if err := hugoServer.Process.Signal(os.Interrupt); err != nil {
		// If interrupt fails, try to kill the process
		return hugoServer.Process.Kill()
	}

	// Wait for the process to exit
	return hugoServer.Wait()
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

// getPost retrieves a specific post by its path
func getPost(path string) (Post, error) {
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
func savePost(path, content string) error {
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

// gitHasChanges checks if there are any uncommitted changes in the git repository
func gitHasChanges() (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = config.HugoSiteDir

	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to check git status: %v", err)
	}

	// If output is empty, there are no changes
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// gitCommitChanges creates a git commit with all changes
func gitCommitChanges() error {
	// Add all changes
	addCmd := exec.Command("git", "add", ".")
	addCmd.Dir = config.HugoSiteDir
	if err := addCmd.Run(); err != nil {
		return fmt.Errorf("failed to add changes: %v", err)
	}

	// Create commit with timestamp
	commitMsg := fmt.Sprintf("Auto-publish: %s", time.Now().Format("2006-01-02 15:04:05"))
	commitCmd := exec.Command("git", "commit", "-m", commitMsg)
	commitCmd.Dir = config.HugoSiteDir

	if err := commitCmd.Run(); err != nil {
		return fmt.Errorf("failed to commit changes: %v", err)
	}

	log.Printf("Created git commit: %s", commitMsg)
	return nil
}

// gitPushChanges pushes the current branch to the remote repository
func gitPushChanges() error {
	// Get current branch name
	branchCmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	branchCmd.Dir = config.HugoSiteDir

	branchOutput, err := branchCmd.Output()
	if err != nil {
		return fmt.Errorf("failed to get current branch: %v", err)
	}

	currentBranch := strings.TrimSpace(string(branchOutput))
	log.Printf("Pushing branch: %s", currentBranch)

	// Push to remote
	pushCmd := exec.Command("git", "push", "origin", currentBranch)
	pushCmd.Dir = config.HugoSiteDir

	if err := pushCmd.Run(); err != nil {
		return fmt.Errorf("failed to push changes: %v", err)
	}

	log.Printf("Successfully pushed changes to remote")
	return nil
}

// publishSite runs the publish command
func publishSite() error {
	// Stop the Hugo server
	if err := stopHugoServer(); err != nil {
		log.Printf("Warning: Failed to stop Hugo server: %v", err)
	}

	// Split the command into parts
	parts := strings.Fields(config.PublishCmd)
	if len(parts) == 0 {
		return fmt.Errorf("invalid publish command")
	}

	// Create the command
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = config.HugoSiteDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Run the command
	log.Printf("Running publish command: %s", config.PublishCmd)
	err := cmd.Run()

	// Restart the Hugo server regardless of publish result
	log.Println("Restarting Hugo server...")
	startHugoServer()

	return err
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
		PreviewURL: fmt.Sprintf("http://localhost:%d/%s", config.HugoServerPort, strings.TrimSuffix(post.Path, ".md")),
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

// handlePublish runs the publish command
func handlePublish(w http.ResponseWriter, r *http.Request) {
	// Only accept POST requests
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Create and push a git commit if there are any changes
	hasChanges, err := gitHasChanges()
	if err != nil {
		log.Printf("Warning: Failed to check git status: %v", err)
	} else if hasChanges {
		log.Println("Git changes detected, creating commit...")
		if err = gitCommitChanges(); err != nil {
			log.Printf("Warning: Failed to create git commit: %v", err)
		} else {
			// Push the git branch if commit was successful
			log.Println("Pushing changes to remote...")
			if err = gitPushChanges(); err != nil {
				log.Printf("Warning: Failed to push changes: %v", err)
			}
		}
	} else {
		log.Println("No git changes detected, skipping commit")
	}

	if err != nil {
		// Return JSON response with error
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	// Run the publish command
	if len(config.PublishCmd) > 0 {
		err := publishSite()
		if err != nil {
			// Return JSON response with error
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false,
				"error":   err.Error(),
			})
			return
		}
	}

	// Return success JSON response
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true,
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
