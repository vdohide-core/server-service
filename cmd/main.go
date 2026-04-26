package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"server-service/internal/config"
	"server-service/internal/db/database"
	"server-service/internal/handlers"
	"server-service/internal/logger"
	"server-service/internal/middleware"
	"server-service/internal/services"
	"syscall"
	"time"

	"github.com/joho/godotenv"
)

func main() {
	log.Println("🚀 Starting Service Server (Cronjobs)")

	// Load .env (optional)
	_ = godotenv.Load()

	// Load config
	config.Load()

	// Init file logger (writes to stdout + rotating log file)
	logCloser, err := logger.Init(config.AppConfig.LogPath)
	if err != nil {
		log.Printf("⚠️ File logging disabled: %v", err)
	} else {
		defer logCloser.Close()
		log.Printf("📝 Logging to: %s (max 25MB per file)", config.AppConfig.LogPath)
	}

	// Connect to MongoDB
	if err := database.Connect(); err != nil {
		log.Fatalf("❌ Failed to connect to MongoDB: %v", err)
	}
	defer database.Disconnect()
	log.Println("✅ MongoDB connected")

	// Get port from environment or use default
	port := config.AppConfig.Port
	if port == "" {
		port = "8084"
	}

	// Setup graceful shutdown context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start WebSocket hub (always needed for UI)
	go handlers.GlobalHub.Run()

	if config.AppConfig.EnableSchedulers {
		// Start space capacity sync scheduler (calc storage usage per space, batch=5, every 1 min)
		go services.StartSpaceCapacitySyncScheduler(ctx)

		// Start domain CNAME verify scheduler (verify pending domains, every 1 min)
		go services.StartDomainVerifyScheduler(ctx)

		// Start file cleanup scheduler (hard delete soft-deleted files, every 1 min)
		go services.StartFileCleanupScheduler(ctx)

		// Start S3 storage cleanup scheduler (purge media/ingest objects from S3, every 1 min)
		go services.StartS3CleanupScheduler(ctx)

		// Start Hetzner auto-scaler (spin up/down download servers based on pending videos)
		go services.StartHetznerScalerScheduler(ctx)

		log.Println("✅ Schedulers enabled (production mode)")
	} else {
		log.Println("⚠️  Schedulers DISABLED (SCHEDULERS=false) — HTTP only mode")
	}

	// Initialize handlers
	logDir := filepath.Dir(config.AppConfig.LogPath)
	h := handlers.NewHandler(handlers.Handler{LogDir: logDir})

	// Start log file watcher (broadcasts changes to WS clients)
	go handlers.WatchLogDir(logDir)

	// Setup HTTP routes
	mux := http.NewServeMux()

	// Route: /health — Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"ok","service":"server-service"}`)
	})

	// Route: /logs — Log list API
	mux.HandleFunc("/logs", h.HandleLogList)
	mux.HandleFunc("/logs/", h.HandleLogFile)

	// Route: /ui — Log viewer web interface
	mux.HandleFunc("/ui", h.HandleUI)

	// Route: /ws — WebSocket (real-time log streaming)
	mux.HandleFunc("/ws", h.HandleWS)

	// Route: /hetzner — Hetzner server management
	mux.HandleFunc("/hetzner/servers", h.HandleHetznerServers)
	mux.HandleFunc("/hetzner/log", h.HandleHetznerLog)

	// Catch-all → 404
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	// Create server
	server := &http.Server{
		Addr:    ":" + port,
		Handler: middleware.CORS(mux),
	}

	// Setup graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("⏹️ Shutting down...")
		shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 5*time.Second)
		defer shutdownCancel()
		server.Shutdown(shutdownCtx)
	}()

	// Start server
	log.Printf("🌐 Server listening on http://localhost:%s", port)
	log.Printf("📍 Endpoints:")
	log.Printf("   GET /health        - Health check")
	log.Printf("   GET /logs          - Log file list")
	log.Printf("   GET /logs/{file}   - Log file reader")
	log.Printf("📋 Cronjobs:")
	log.Printf("   ⏱️  Space capacity sync     - every 1 min (batch=5)")
	log.Printf("   ⏱️  Domain CNAME verify     - every 1 min (batch=5)")
	log.Printf("   ⏱️  File cleanup            - every 1 min (batch=5)")
	log.Printf("   ⏱️  S3 storage cleanup      - every 1 min")

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("❌ Server error: %v", err)
	}

	log.Println("👋 Server stopped")
}
