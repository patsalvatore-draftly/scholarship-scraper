package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/gocolly/colly/v2"
	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

var (
	_ctx      context.Context
	_cancel   context.CancelFunc
	isRunning bool
	db        *sql.DB
	lock      sync.Mutex // Prevent concurrent writes
)

// Start Background Task
func startHandler(w http.ResponseWriter, _ *http.Request) {
	if isRunning {
		http.Error(w, "Task already running", http.StatusConflict)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	_cancel = cancel
	isRunning = true
	go scrape(ctx)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Started background task"))
}

// Stop Background Task
func stopHandler(w http.ResponseWriter, r *http.Request) {
	if !isRunning {
		http.Error(w, "No running task to stop", http.StatusBadRequest)
		return
	}

	_cancel()
	isRunning = false
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Stopped background task"))
}

// Initialize database
func initDB() *sql.DB {
	db, err := sql.Open("sqlite3", "database?_journal_mode=WAL")
	if err != nil {
		log.Fatal(err)
	}

	// Set connection limits
	db.SetMaxOpenConns(1) // Ensures a single writer at a time
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	// Enable WAL mode
	_, err = db.Exec("PRAGMA journal_mode=WAL;")
	if err != nil {
		log.Fatal("Failed to enable WAL mode:", err)
	}

	// Create table if it doesn't exist
	query := `CREATE TABLE IF NOT EXISTS scraped_data (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT UNIQUE,
		content TEXT,
		status_code INTEGER,
		timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	_, err = db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}
	query = `CREATE TABLE IF NOT EXISTS urls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		url TEXT UNIQUE,
		status TEXT
	);`
	_, err = db.Exec(query)
	if err != nil {
		log.Fatal(err)
	}

	return db
}

func scrape(ctx context.Context) {
	c := colly.NewCollector()

	rows, err := db.Query("SELECT * FROM urls")
	if err != nil {
		log.Fatal(map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	var results []map[string]any

	for rows.Next() {
		var id int
		var url string
		var status string

		// Scan the row into variables
		if err := rows.Scan(&id, &url, &status); err != nil {
			log.Fatal(err)
		}

		// Print the row data
		data := map[string]any{
			"id":     id,
			"url":    url,
			"status": status,
		}
		results = append(results, data)
	}

	c.Limit(&colly.LimitRule{
		Parallelism: 1,
		Delay:       5 * time.Second,
		RandomDelay: time.Duration(rand.Intn(3)+1) * time.Second,
	})

	// On every a element which has href attribute call callback
	c.OnHTML("script#\\__NEXT_DATA__", func(e *colly.HTMLElement) {
		url := e.Request.URL

		// Lock before writing to the database
		lock.Lock()
		defer lock.Unlock()

		tx, err := db.BeginTx(context.Background(), &sql.TxOptions{})
		if err != nil {
			fmt.Println("Error saving to DB:", err)
			return
		}

		_, err = tx.Exec("INSERT OR IGNORE INTO scraped_data (url, content, status_code) VALUES (?, ?, ?)", url.String(), e.Text, 200)
		if err != nil {
			fmt.Println("Error saving scraped data to DB:", err)
			tx.Rollback()
			return
		}

		_, err = tx.Exec("UPDATE urls SET status = ? WHERE url = ?", "complete", url.String())
		if err != nil {
			fmt.Println("Error saving url data to DB:", err)
			tx.Rollback()
			return
		}

		fmt.Println("Successfully saved data for", url.String())
		tx.Commit()
	})

	// Set error handler
	c.OnError(func(r *colly.Response, err error) {
		fmt.Println("Request URL:", r.Request.URL, "failed with response:", r.StatusCode, "\nError:", err.Error())
		_cancel()
		isRunning = false
	})

	// Before making a request print "Visiting ..."
	c.OnRequest(func(r *colly.Request) {
		fmt.Println("Visiting", r.URL.String())
	})

	c.OnResponse(func(r *colly.Response) {
		log.Printf("OnResponse %s", r.Request.URL.String())
	})

	for _, result := range results {
		select {
		case <-ctx.Done():
			fmt.Println("Scraper stopped")
			return
		default:
			url, ok := result["url"].(string)
			if !ok {
				fmt.Println("Invalid URL data:", result)
				continue
			}

			status, ok := result["status"].(string)
			if !ok {
				fmt.Println("Invalid status data:", result)
				continue
			}

			if status == "complete" {
				fmt.Println("Already processed:", url)
				continue
			}

			c.Visit(url)
		}
	}
	fmt.Println("scraping complete, thank god")
}

type Url struct {
	Id     int
	Url    string
	Status string
}

func WriteJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

func main() {
	// Initialize DB
	db = initDB()
	defer db.Close()

	router := chi.NewMux()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)
	router.Use(middleware.Logger)
	router.Use(middleware.Recoverer)
	router.Use(middleware.Timeout(2 * 60 * time.Second))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	router.Get("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello world")) })
	router.Get("/sql", func(w http.ResponseWriter, r *http.Request) {
		queryParam := r.URL.Query().Get("q") // Get the 'q' query parameter

		rows, err := db.Query(queryParam)
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		// Get column names
		columns, err := rows.Columns()
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// Prepare a slice of maps to hold results
		var results []map[string]interface{}

		// Iterate over rows
		for rows.Next() {
			// Create a slice of interface{} to hold column values
			values := make([]interface{}, len(columns))
			valuePtrs := make([]interface{}, len(columns))

			// Assign pointers to values
			for i := range values {
				valuePtrs[i] = &values[i]
			}

			// Scan row into value pointers
			if err := rows.Scan(valuePtrs...); err != nil {
				WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}

			// Convert row into a map
			rowMap := make(map[string]interface{})
			for i, colName := range columns {
				rowMap[colName] = values[i]
			}

			results = append(results, rowMap)
		}

		// Check for errors after iteration
		if err := rows.Err(); err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// Return JSON response
		WriteJSON(w, http.StatusOK, map[string]any{"data": results})
	})

	router.Get("/backfill", func(w http.ResponseWriter, r *http.Request) {
		// Get all JSON files under /data/*/unknowns.json
		count := 0
		files, err := filepath.Glob("data/*/unknowns.json")
		if err != nil {
			log.Fatal(err)
		}

		if len(files) == 0 {
			fmt.Println("No JSON files found.")
			return
		}

		// Process each file
		for _, file := range files {
			fmt.Println("Processing:", file)
			// Read JSON file
			data, err := os.ReadFile(file)
			if err != nil {
				log.Printf("Error reading file %s: %v\n", file, err)
				return
			}

			// Parse JSON
			var jsonData []map[string]interface{}
			err = json.Unmarshal(data, &jsonData)
			if err != nil {
				log.Printf("Error parsing JSON in file %s: %v\n", file, err)
				return
			}

			// Insert data into SQLite
			for _, entry := range jsonData {
				url, ok := entry["programTitleSlug"].(string)
				if !ok {
					log.Printf("Skipping entry in %s (missing 'url')\n", file)
					continue
				}

				// Insert into database
				_, err = db.Exec("INSERT INTO urls (url, status) VALUES (?, ?)", "https://bigfuture.collegeboard.org/scholarships/"+url, "")
				if err != nil {
					log.Printf("Failed to insert URL %s: %v\n", url, err)
					return
				}
				log.Printf("inserted URL %s: %v\n", url)
				count++
			}

		}

		fmt.Println(count)
		w.Write([]byte("done"))
	})

	router.Get("/start", startHandler)
	router.Get("/stop", stopHandler)

	srv := &http.Server{
		Addr:    ":" + "8080",
		Handler: router,
	}

	go func() {
		fmt.Printf("%s", fmt.Sprintf("Listening on port %s... 🚀 \n", "8080"))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err.Error())
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Printf("Shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown:", err)
	}

	log.Printf("Server exiting")
}
