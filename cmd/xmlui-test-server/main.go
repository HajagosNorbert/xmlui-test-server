package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"xmlui-test-server"
)

// launchBrowser opens the specified URL in the user's default browser
func launchBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default: // Unix-like
		cmd = "xdg-open"
		args = []string{url}
	}

	err := exec.Command(cmd, args...).Start()
	if err != nil {
		log.Printf("Failed to launch browser: %v", err)
	}
}

// injectPgPort injects or overrides the port in a Postgres connection string (URL or DSN format)
func injectPgPort(pgConnStr, pgPort string) string {
	if pgConnStr == "" || pgPort == "" {
		return pgConnStr
	}
	if strings.HasPrefix(pgConnStr, "postgres://") || strings.HasPrefix(pgConnStr, "postgresql://") {
		u, err := url.Parse(pgConnStr)
		if err == nil {
			if u.Port() == "" || u.Port() != pgPort {
				u.Host = u.Hostname() + ":" + pgPort
				return u.String()
			}
		}
		return pgConnStr
	}
	// DSN format: add or replace port=...
	re := regexp.MustCompile(`port=\d+`)
	if re.MatchString(pgConnStr) {
		return re.ReplaceAllString(pgConnStr, "port="+pgPort)
	}
	return pgConnStr + " port=" + pgPort
}

func main() {
	// Set custom flag usage to display double dashes for word options
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
		flag.VisitAll(func(f *flag.Flag) {
			prefix := "-"
			// Use double dash for multi-character flags
			if len(f.Name) > 1 {
				prefix = "--"
			}
			fmt.Fprintf(flag.CommandLine.Output(), "  %s%s: %s\n", prefix, f.Name, f.Usage)
		})
	}

	// Set up command line flags with long and short versions
	var portValue string
	flag.StringVar(&portValue, "port", "8080", "Port to run the server on")
	flag.StringVar(&portValue, "p", "8080", "Port to run the server on (shorthand)")
	extension := flag.String("extension", "", "Path to SQLite extension to load")
	apiDesc := flag.String("api", "", "Path to API description file")
	dbPath := flag.String("db", "data.db", "Path to SQLite database file")
	showResponses := flag.Bool("show-responses", false, "Enable logging of SQL query responses")
	pgConnStr := flag.String("pg-conn", "", "PostgreSQL connection string (if provided, use PostgreSQL instead of SQLite)")
	pgPort := flag.String("pg-port", "", "PostgreSQL port (optional, overrides port in --pg-conn if provided)")
	clientDir := flag.String("client", "client", "Directory containing client files (SPA)")

	// Short-form alias for show-responses
	var shortShowResponses bool
	flag.BoolVar(&shortShowResponses, "s", false, "Enable logging of SQL query responses (shorthand)")

	flag.Parse()

	// Set up logging
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	log.Println("Server starting...")

	// Print current working directory
	pwd, err := os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Working directory: %s", pwd)

	// Initialize server
	showResponsesEnabled := *showResponses || shortShowResponses
	finalPgConnStr := injectPgPort(*pgConnStr, *pgPort)

	config := xmluibackend.ServerConfig{
		DBPath:        *dbPath,
		PgConnStr:     finalPgConnStr,
		ExtensionPath: *extension,
		APIDescPath:   *apiDesc,
		ShowResponses: showResponsesEnabled,
	}

	server, err := xmluibackend.NewServer(config)
	if err != nil {
		log.Fatal(err)
	}
	defer server.Close()

	// Create router
	mux := http.NewServeMux()

	// Setup routes
	server.SetupRoutes(mux, *clientDir)

	// Log server settings
	log.Printf("Server configuration:")
	log.Printf("- Port: %s", portValue)
	log.Printf("- API Description: %s", *apiDesc)
	log.Printf("- Extension: %s", *extension)
	log.Printf("- Show Responses: %v", showResponsesEnabled)
	log.Printf("- Client Directory: %s", *clientDir)
	if *pgConnStr != "" {
		log.Printf("- Database: PostgreSQL")
	} else {
		os.Setenv("STEAMPIPE_CACHE", "false")
		log.Printf("- Database: SQLite (data.db)")
	}

	// Start server
	log.Printf("Server listening on localhost:%s...", portValue)
	if err := http.ListenAndServe("127.0.0.1:"+portValue, xmluibackend.CORSMiddleware(mux)); err != nil {
		log.Fatal(err)
	}
}
