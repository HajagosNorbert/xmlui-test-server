package xmluibackend

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	_ "github.com/lib/pq"  // PostgreSQL driver
	_ "modernc.org/sqlite" // SQLite driver (pure Go, no CGO)
)

// ===== Data Structures =====

type QueryRequest struct {
	SQL    string        `json:"sql"`
	Params []interface{} `json:"params"`
}

// API Description structures
type APIDescription struct {
	APIVersion  string               `json:"apiVersion"`
	Name        string               `json:"name"`
	Description string               `json:"description"`
	BasePath    string               `json:"basePath"`
	Endpoints   []EndpointDefinition `json:"endpoints"`
}

type EndpointDefinition struct {
	Path    string                      `json:"path"`
	Methods map[string]MethodDefinition `json:"methods"`
}

type MethodDefinition struct {
	Description string   `json:"description"`
	SQL         string   `json:"sql,omitempty"`
	SQLFile     string   `json:"sqlFile,omitempty"`
	Params      []string `json:"params,omitempty"`
}

type Server struct {
	db            *sql.DB
	apiDesc       *APIDescription
	apiDescPath   string                    // Path to the API description file
	pathRegexps   map[string]*regexp.Regexp // Cache for compiled path regexps
	showResponses bool                      // Flag to enable/disable response logging
	dbType        string                    // Type of database: "sqlite" or "postgres"
	mu            sync.Mutex                // Mutex to serialize DB access
}

// ServerConfig holds configuration options for creating a new Server
type ServerConfig struct {
	DBPath         string // Path to SQLite database file
	PgConnStr      string // PostgreSQL connection string
	ExtensionPath  string // Path to SQLite extension (not supported with pure Go driver)
	APIDescPath    string // Path to API description JSON file
	ShowResponses  bool   // Enable logging of SQL query responses
}

// ===== Server Initialization =====

// NewServer creates a new Server instance with the given configuration
func NewServer(config ServerConfig) (*Server, error) {
	var db *sql.DB
	var err error
	var dbType string

	// Determine which database to use
	if config.PgConnStr != "" {
		// Use PostgreSQL if PgConnStr is provided
		log.Println("Using PostgreSQL database")
		db, err = sql.Open("postgres", config.PgConnStr)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to PostgreSQL: %w", err)
		}
		dbType = "postgres"
	} else {
		// Default to SQLite
		log.Println("Using SQLite database")
		// modernc.org/sqlite uses "sqlite" as the driver name
		db, err = sql.Open("sqlite", config.DBPath)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to SQLite: %w", err)
		}
		dbType = "sqlite"

		// SQLite specific configurations
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)

		// Note: modernc.org/sqlite (pure Go) doesn't support C extensions
		// Extensions are not available with this pure Go SQLite driver
		if config.ExtensionPath != "" {
			log.Printf("Warning: Extension loading is not supported with zombiezen.com/go/sqlite")
			log.Printf("The pure Go SQLite driver does not support C extensions")
		}
	}

	// Initialize the server
	server := &Server{
		db:            db,
		pathRegexps:   make(map[string]*regexp.Regexp),
		showResponses: config.ShowResponses,
		dbType:        dbType,
		apiDescPath:   config.APIDescPath,
		mu:            sync.Mutex{},
	}

	// Load the API description if provided
	if config.APIDescPath != "" {
		if _, err := os.Stat(config.APIDescPath); os.IsNotExist(err) {
			log.Printf("API description file not found: %s", config.APIDescPath)
		} else {
			apiDesc, err := loadAPIDescription(config.APIDescPath)
			if err != nil {
				log.Printf("Warning: Failed to load API description: %v", err)
			} else {
				server.apiDesc = &apiDesc
				log.Printf("API description loaded successfully: %s (v%s)", apiDesc.Name, apiDesc.APIVersion)

				// Precompile the path regexps for faster matching
				for _, endpoint := range apiDesc.Endpoints {
					pathRegexp := pathToRegexp(endpoint.Path)
					server.pathRegexps[endpoint.Path] = regexp.MustCompile(pathRegexp)
				}
			}
		}
	}

	return server, nil
}

// Close closes the database connection
func (s *Server) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// ===== API Description Handling =====

// Load API description from file
func loadAPIDescription(filePath string) (APIDescription, error) {
	var apiDesc APIDescription
	data, err := os.ReadFile(filePath)
	if err != nil {
		return apiDesc, fmt.Errorf("failed to read API description file: %w", err)
	}

	err = json.Unmarshal(data, &apiDesc)
	if err != nil {
		return apiDesc, fmt.Errorf("failed to parse API description JSON: %w", err)
	}

	return apiDesc, nil
}

// Convert a path template to a regexp
// Example: "/clients/:id" -> "^/clients/([^/]+)$"
func pathToRegexp(path string) string {
	// Escape any special regexp characters in the path
	escaped := regexp.QuoteMeta(path)

	// Replace :paramName with a capturing group
	re := regexp.MustCompile(`:([^/]+)`)
	regexpPath := re.ReplaceAllString(escaped, "([^/]+)")

	// Add start and end anchors
	return fmt.Sprintf("^%s$", regexpPath)
}

// Extract path parameters from a URL based on the endpoint path template
// Example: extractPathParams("/clients/123", "/clients/:id") -> {"id": "123"}
func extractPathParams(requestPath string, endpointPath string, re *regexp.Regexp) map[string]string {
	params := make(map[string]string)

	// Extract param names from the path template
	paramNames := make([]string, 0)
	pathParts := strings.Split(endpointPath, "/")
	for _, part := range pathParts {
		if strings.HasPrefix(part, ":") {
			paramNames = append(paramNames, part[1:])
		}
	}

	// Extract values using regexp
	matches := re.FindStringSubmatch(requestPath)
	if len(matches) > 1 {
		// First match is the whole string, subsequent matches are capture groups
		for i, name := range paramNames {
			if i+1 < len(matches) {
				params[name] = matches[i+1]
			}
		}
	}

	return params
}

// Find the matching endpoint for a request path
func (s *Server) findMatchingEndpoint(requestPath string) (*EndpointDefinition, map[string]string) {
	if s.apiDesc == nil {
		return nil, nil
	}

	// Strip base path if present
	basePath := s.apiDesc.BasePath
	if basePath != "" && strings.HasPrefix(requestPath, basePath) {
		requestPath = strings.TrimPrefix(requestPath, basePath)
		if requestPath == "" {
			requestPath = "/"
		}
	}

	// Normalize the path by removing trailing slashes
	normalizedPath := strings.TrimSuffix(requestPath, "/")
	if normalizedPath == "" {
		normalizedPath = "/"
	}

	// First try exact match with normalized path
	for _, endpoint := range s.apiDesc.Endpoints {
		re, exists := s.pathRegexps[endpoint.Path]
		if !exists {
			// This shouldn't happen as we precompile all regexps
			log.Printf("Warning: No regexp for path %s", endpoint.Path)
			continue
		}

		if re.MatchString(normalizedPath) {
			params := extractPathParams(normalizedPath, endpoint.Path, re)
			return &endpoint, params
		}
	}

	// If we reach here, try matching with the original path as a fallback
	if normalizedPath != requestPath {
		for _, endpoint := range s.apiDesc.Endpoints {
			re, exists := s.pathRegexps[endpoint.Path]
			if !exists {
				continue
			}

			if re.MatchString(requestPath) {
				params := extractPathParams(requestPath, endpoint.Path, re)
				return &endpoint, params
			}
		}
	}

	return nil, nil
}

// ===== Parameter Extraction =====

// Extract query parameters from request URL
func extractQueryParams(r *http.Request) map[string]string {
	queryParams := make(map[string]string)
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			queryParams[key] = values[0]
		}
	}
	return queryParams
}

// Extract JSON body parameters from request
func extractBodyParams(r *http.Request) (map[string]interface{}, error) {
	bodyParams := make(map[string]interface{})

	if r.Body == nil {
		return bodyParams, nil
	}

	var bodyBuffer bytes.Buffer
	bodyReader := io.TeeReader(r.Body, &bodyBuffer)

	bodyBytes, err := io.ReadAll(bodyReader)
	if err != nil {
		return bodyParams, err
	}

	if len(bodyBytes) > 0 {
		err = json.Unmarshal(bodyBytes, &bodyParams)
		if err != nil {
			return bodyParams, err
		}
	}

	// Reset r.Body for potential future use
	r.Body = io.NopCloser(&bodyBuffer)

	return bodyParams, nil
}

// ===== SQL Execution =====

// ExecuteQuery executes SQL query and returns results as maps
func (s *Server) ExecuteQuery(sqlQuery string, params []interface{}) ([]map[string]interface{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Log the SQL query (just once)
	log.Printf("SQL: %s", sqlQuery)

	// Handle PostgreSQL parameter placeholders ($1, $2, etc.) vs SQLite (?, ?, etc.)
	if s.dbType == "postgres" {
		// Replace ? with $1, $2, etc. for PostgreSQL
		for i := 1; i <= len(params); i++ {
			sqlQuery = strings.Replace(sqlQuery, "?", fmt.Sprintf("$%d", i), 1)
		}
	}

	// Execute the query
	rows, err := s.db.Query(sqlQuery, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Get column information
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	// Process result rows
	var result []map[string]interface{}
	for rows.Next() {
		// Create values slice with appropriate length
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range columns {
			valuePtrs[i] = &values[i]
		}

		// Scan the row into values
		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, err
		}

		// Create a map for this row
		entry := make(map[string]interface{})
		for i, col := range columns {
			var v interface{}
			val := values[i]
			b, ok := val.([]byte)
			if ok {
				v = string(b)
			} else {
				v = val
			}
			entry[col] = v
		}

		// Add the row to the result
		result = append(result, entry)
	}

	// Check for errors after iteration
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Log the response if enabled
	if s.showResponses {
		// Response logging is done in sendJSONResponse
	}

	return result, nil
}

// ===== HTTP Response Handling =====

// Send JSON response with the given status code
func (s *Server) sendJSONResponse(w http.ResponseWriter, data interface{}, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	// Generate JSON response
	responseJSON, err := json.Marshal(data)
	if err != nil {
		log.Printf("Error encoding JSON response: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	// Log the response if enabled - this is the ONLY place where responses should be logged
	if s.showResponses {
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, responseJSON, "", "  "); err != nil {
			log.Printf("Error prettifying JSON for logging: %v", err)
		} else {
			log.Printf("Response: %s", prettyJSON.String())
		}
	}

	// Send the response
	if _, err := w.Write(responseJSON); err != nil {
		log.Printf("Error writing response: %v", err)
	}
}

// Send error response with the given status code
func sendErrorResponse(w http.ResponseWriter, message string, statusCode int) {
	log.Printf("Error: %s (Status: %d)", message, statusCode)
	http.Error(w, message, statusCode)
}

// SetContentType sets appropriate MIME type based on file extension
func SetContentType(w http.ResponseWriter, filename string) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case ".json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	case ".png":
		w.Header().Set("Content-Type", "image/png")
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".gif":
		w.Header().Set("Content-Type", "image/gif")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	case ".ico":
		w.Header().Set("Content-Type", "image/x-icon")
	case ".woff":
		w.Header().Set("Content-Type", "font/woff")
	case ".woff2":
		w.Header().Set("Content-Type", "font/woff2")
	case ".ttf":
		w.Header().Set("Content-Type", "font/ttf")
	case ".eot":
		w.Header().Set("Content-Type", "application/vnd.ms-fontobject")
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
}

// ===== Request Handlers =====

// HandleAPI handles API requests based on the API description
func (s *Server) HandleAPI(w http.ResponseWriter, r *http.Request) {
	log.Printf("API: %s %s", r.Method, r.URL.Path)

	if s.apiDesc == nil {
		sendErrorResponse(w, "API description not loaded", http.StatusInternalServerError)
		return
	}

	// Find the matching endpoint
	endpoint, pathParams := s.findMatchingEndpoint(r.URL.Path)
	if endpoint == nil {
		http.NotFound(w, r)
		return
	}

	// Check if the method is supported
	methodDef, exists := endpoint.Methods[r.Method]
	if !exists {
		log.Printf("Method %s not allowed for endpoint %s", r.Method, endpoint.Path)
		sendErrorResponse(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract parameters
	queryParams := extractQueryParams(r)

	bodyParams, err := extractBodyParams(r)
	if err != nil {
		log.Printf("Warning: Failed to parse request body as JSON: %v", err)
	}

	// Prepare SQL query
	sqlQuery := ""

	// Check if SQL should be loaded from a file
	if methodDef.SQLFile != "" {
		// Determine the API description file's directory to make relative paths work
		apiDir := filepath.Dir(s.apiDescPath)

		// Build the SQL file path relative to the API description file
		sqlFilePath := filepath.Join(apiDir, methodDef.SQLFile)
		log.Printf("Loading SQL from file: %s", sqlFilePath)

		// Read the SQL file
		sqlBytes, err := os.ReadFile(sqlFilePath)
		if err != nil {
			sendErrorResponse(w, fmt.Sprintf("Failed to read SQL file: %v", err), http.StatusInternalServerError)
			return
		}

		// Use the file contents as the SQL query
		sqlQuery = string(sqlBytes)
	} else {
		// Use the inline SQL from the API definition
		sqlQuery = methodDef.SQL
	}

	// Replace named parameters with ? placeholders and build params array
	var sqlParams []interface{}

	// If we have defined params, use them in order
	if len(methodDef.Params) > 0 {
		for _, paramName := range methodDef.Params {
			// Check path params first, then query params, then body params
			if value, ok := pathParams[paramName]; ok {
				sqlParams = append(sqlParams, value)
				sqlQuery = strings.Replace(sqlQuery, ":"+paramName, "?", 1)
			} else if value, ok := queryParams[paramName]; ok {
				sqlParams = append(sqlParams, value)
				sqlQuery = strings.Replace(sqlQuery, ":"+paramName, "?", 1)
			} else if value, ok := bodyParams[paramName]; ok {
				sqlParams = append(sqlParams, value)
				sqlQuery = strings.Replace(sqlQuery, ":"+paramName, "?", 1)
			} else {
				// Parameter not found, add nil
				sqlParams = append(sqlParams, nil)
				sqlQuery = strings.Replace(sqlQuery, ":"+paramName, "?", 1)
			}
		}
	}

	// Execute the query
	result, err := s.ExecuteQuery(sqlQuery, sqlParams)
	if err != nil {
		sendErrorResponse(w, fmt.Sprintf("Database error: %v", err), http.StatusInternalServerError)
		return
	}

	// Return response
	s.sendJSONResponse(w, result, http.StatusOK)
}

// HandleQuery handles direct SQL query requests
func (s *Server) HandleQuery(w http.ResponseWriter, r *http.Request) {
	log.Printf("Query: %s", r.URL.Path)

	if r.Method != "POST" {
		sendErrorResponse(w, "Only POST method is allowed", http.StatusMethodNotAllowed)
		return
	}

	// Use io.TeeReader to log the body while still allowing it to be read
	var bodyBuffer bytes.Buffer
	teeReader := io.TeeReader(r.Body, &bodyBuffer)

	// Read the body into a buffer
	_, err := io.ReadAll(teeReader)
	if err != nil {
		sendErrorResponse(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}

	// Decode the body into the QueryRequest struct
	var req QueryRequest
	if err := json.NewDecoder(&bodyBuffer).Decode(&req); err != nil {
		sendErrorResponse(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Execute the query
	result, err := s.ExecuteQuery(req.SQL, req.Params)
	if err != nil {
		sendErrorResponse(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Return response
	s.sendJSONResponse(w, result, http.StatusOK)
}

// HandleProxy handles proxy requests
func (s *Server) HandleProxy(w http.ResponseWriter, r *http.Request) {
	// 1. Parse off the part after "/proxy/".
	targetPath := strings.TrimPrefix(r.URL.Path, "/proxy/")
	targetQuery := r.URL.RawQuery

	// 2. Split off the first segment as the actual host.
	pathParts := strings.SplitN(targetPath, "/", 2)
	hostPart := pathParts[0]

	// 3. The remainder is your path on that host.
	var subPath string
	if len(pathParts) > 1 {
		subPath = "/" + pathParts[1]
	} else {
		subPath = "/"
	}

	// 4. Construct a "bare" target with no path so the default Director won't double up paths.
	rawTarget := "https://" + hostPart
	targetURL, err := url.Parse(rawTarget)
	if err != nil {
		sendErrorResponse(w, "Invalid target URL: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 5. Create the reverse proxy.
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// 6. Update the inbound request with subPath and query
	r.URL.Scheme = targetURL.Scheme
	r.URL.Host = targetURL.Host
	r.URL.Path = subPath
	r.URL.RawQuery = targetQuery

	// 7. (Optional) Reassign the Host header to match target
	r.Host = targetURL.Host

	// 8. Finally, run the proxy
	proxy.ServeHTTP(w, r)
}

// CORSMiddleware adds CORS headers to responses
func CORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// CreateStaticFileHandler creates a handler for serving static files with SPA routing support
func CreateStaticFileHandler(staticDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Received request for: %s", r.URL.Path)

		// Try to serve static files from client directory
		filePath := filepath.Join(staticDir, r.URL.Path)

		// If the path is a directory, try index.html inside it
		if strings.HasSuffix(r.URL.Path, "/") {
			filePath = filepath.Join(filePath, "index.html")
		}

		log.Printf("Trying to serve: %s", filePath)

		// Check if file exists
		if _, err := os.Stat(filePath); err == nil {
			// File exists, set appropriate MIME type and serve it
			SetContentType(w, filePath)
			http.ServeFile(w, r, filePath)
			return
		}

		// For SPA routing, if no static file found, serve the main index.html
		// This allows client-side routing to work
		if r.URL.Path != "/" {
			log.Printf("File not found, serving index.html for Single Page Application routing: %s", r.URL.Path)
			indexPath := filepath.Join(staticDir, "index.html")
			if _, err := os.Stat(indexPath); err == nil {
				SetContentType(w, indexPath)
				http.ServeFile(w, r, indexPath)
				return
			}
		}

		// Fallback: serve root index.html
		rootIndexPath := filepath.Join(staticDir, "index.html")
		if _, err := os.Stat(rootIndexPath); err == nil {
			SetContentType(w, rootIndexPath)
			http.ServeFile(w, r, rootIndexPath)
			return
		}

		// If no index.html found, return 404
		log.Printf("File not found: %s", filePath)
		http.NotFound(w, r)
	}
}

// SetupRoutes configures the HTTP routes for the server
func (s *Server) SetupRoutes(mux *http.ServeMux, clientDir string) {
	// Handle API routes first (to match /api/* before static files)
	if s.apiDesc != nil {
		apiBasePath := s.apiDesc.BasePath
		if !strings.HasSuffix(apiBasePath, "/") {
			apiBasePath += "/"
		}
		mux.HandleFunc(apiBasePath, s.HandleAPI)
	}

	// Handle proxy next
	mux.HandleFunc("/proxy/", s.HandleProxy)

	// Then handle query endpoint
	mux.HandleFunc("/query", s.HandleQuery)

	// Handle static files and SPA routing
	mux.HandleFunc("/", CreateStaticFileHandler(clientDir))
}
