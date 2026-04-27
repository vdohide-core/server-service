package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"server-service/internal/db/database"
	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ─── Hetzner Auto-Scaler ──────────────────────────────────────────────

const (
	hetznerAPIBase  = "https://api.hetzner.cloud/v1"
	hetznerLabel    = "managed-by=vdohide-scaler"
	hetznerLabelKey = "managed-by"
	hetznerLabelVal = "vdohide-scaler"
)

// idleTracker tracks when the system first became idle (no pending videos).
var idleTracker struct {
	sync.Mutex
	since *time.Time
}

// ── Hetzner API types ─────────────────────────────────────────────────

type hetznerServer struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// IPv4 is a convenience helper to get the server's public IP.
func (s *hetznerServer) IPv4() string {
	return s.PublicNet.IPv4.IP
}

type hetznerListResp struct {
	Servers []hetznerServer `json:"servers"`
}

type hetznerCreateReq struct {
	Name       string            `json:"name"`
	ServerType string            `json:"server_type"`
	Image      string            `json:"image"`
	Location   string            `json:"location"`
	UserData   string            `json:"user_data"`
	SSHKeys    []string          `json:"ssh_keys,omitempty"`
	Labels     map[string]string `json:"labels"`
}

// ─── Main Sync ────────────────────────────────────────────────────────

// SyncHetznerScaler checks pending video_process records and scales
// Hetzner servers up or down. Config is loaded from the
// "hetzner_auto_scale" setting in the database each run.
func SyncHetznerScaler() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── 1. Load config from DB ────────────────────────────────────────
	cfg := GetHetznerAutoScale(ctx)
	if cfg == nil || !cfg.IsValid() {
		return nil // setting missing, disabled, or missing required fields
	}

	// ── 2. Count pending files grouped by sourceType ─────────────
	// slow = missav (HLS heavy) → SlowPerServer jobs/server
	// fast = upload/direct/others → FastPerServer jobs/server
	countPipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "status", Value: models.FileStatusWaiting},
			{Key: "clonedFrom", Value: bson.D{{Key: "$exists", Value: false}}},
			{Key: "metadata.trashedAt", Value: bson.D{{Key: "$exists", Value: false}}},
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: false}}},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "medias"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_medias"},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "video_process"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_vp"},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "ingests"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_ingests"},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "_medias", Value: bson.D{{Key: "$size", Value: 0}}},
			{Key: "_vp", Value: bson.D{{Key: "$size", Value: 0}}},
			{Key: "$or", Value: bson.A{
				bson.D{{Key: "_ingests.0", Value: bson.D{{Key: "$exists", Value: true}}}},
				bson.D{{Key: "metadata.source", Value: bson.D{{Key: "$exists", Value: true}}}},
			}},
		}}},
		// Group by sourceType
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: "$metadata.sourceType"},
			{Key: "count", Value: bson.D{{Key: "$sum", Value: 1}}},
		}}},
	}

	countCur, err := database.Files().Aggregate(ctx, countPipeline)
	if err != nil {
		return fmt.Errorf("count pending aggregate: %w", err)
	}
	var countResult []struct {
		SourceType string `bson:"_id"`
		Count      int64  `bson:"count"`
	}
	if err := countCur.All(ctx, &countResult); err != nil {
		return fmt.Errorf("count pending decode: %w", err)
	}
	countCur.Close(ctx)

	// แยก slow (missav) กับ fast (upload/direct/others)
	var slowPending, fastPending int64
	for _, r := range countResult {
		if r.SourceType == "missav" {
			slowPending += r.Count
		} else {
			fastPending += r.Count
		}
	}
	pending := slowPending + fastPending

	// คำนวณ server ที่ต้องการ แบบ weighted
	slowNeeded := 0
	if slowPending > 0 {
		slowNeeded = int(math.Ceil(float64(slowPending) / float64(cfg.SlowPerServer)))
	}
	fastNeeded := 0
	if fastPending > 0 {
		fastNeeded = int(math.Ceil(float64(fastPending) / float64(cfg.FastPerServer)))
	}

	// ── 3. List current managed servers ──────────────────────────────
	current, err := hetznerListServers(cfg.APIToken)
	if err != nil {
		return fmt.Errorf("list servers: %w", err)
	}

	log.Printf("🖥️  Hetzner scaler: pending=%d (slow/missav=%d, fast/others=%d) current_servers=%d",
		pending, slowPending, fastPending, len(current))

	// ── 4. Scale decision ─────────────────────────────────────────────
	if pending > 0 {
		// Reset idle timer
		idleTracker.Lock()
		idleTracker.since = nil
		idleTracker.Unlock()

		needed := slowNeeded + fastNeeded
		if needed > cfg.MaxServers {
			needed = cfg.MaxServers
		}
		if needed < 1 {
			needed = 1
		}

		diff := needed - len(current)

		switch {
		case diff > 0:
			// Guard: if any server is still initializing, wait for it to come online
			// before adding more — avoids over-provisioning during boot time.
			initializingCount := 0
			for _, s := range current {
				if s.Status == "initializing" {
					initializingCount++
				}
			}
			if initializingCount > 0 {
				log.Printf("🖥️  Scale UP paused: %d server(s) still initializing, waiting...", initializingCount)
				break
			}

			log.Printf("🖥️  Scale UP: creating %d server(s) (pending=%d needed=%d)", diff, pending, needed)
			ts := time.Now().Unix()
			for i := 0; i < diff; i++ {
				// unique name: vdohide-dl-{timestamp}-{i}
				name, err := hetznerCreateServerNamed(cfg, fmt.Sprintf("vdohide-dl-%d-%d", ts, i))
				if err != nil {
					log.Printf("  ⚠️ create server failed: %v", err)
					continue
				}
				log.Printf("  ✅ Created: %s", name)
			}

		case diff < 0:
			toDelete := -diff
			log.Printf("🖥️  Scale DOWN: need to shed %d server(s) (needed=%d current=%d) — checking billing boundary...", toDelete, needed, len(current))
			deleted := 0
			for i := 0; i < len(current) && deleted < toDelete; i++ {
				s := current[i]

				// ── เช็ค billing boundary ──────────────────────────────────
				// Hetzner คิดเงินเป็นชั่วโมง → ลบเฉพาะเมื่อใกล้สิ้นชั่วโมง
				minutesIntoHour := int(time.Since(s.Created).Minutes()) % 60
				minutesUntilBoundary := 60 - minutesIntoHour
				inWindow := minutesIntoHour >= (60 - cfg.DeletionWindowMinutes)

				if !inWindow {
					log.Printf("  ⏳ Skip %s — %dm into billing hour, delete window opens in ~%dm",
						s.Name, minutesIntoHour, minutesUntilBoundary)
					continue
				}

				// ── เช็ค active jobs ───────────────────────────────────────
				activeJobs, err := database.VideoProcess().CountDocuments(ctx, bson.M{
					"workerId": bson.M{"$regex": "^" + s.Name + "@"},
					"status":   bson.M{"$in": bson.A{models.ProcessStatusPending, models.ProcessStatusProcessing}},
				})
				if err != nil {
					log.Printf("  ⚠️ check active jobs for %s: %v", s.Name, err)
					continue
				}
				if activeJobs > 0 {
					log.Printf("  ⏳ Skip %s — still has %d active job(s)", s.Name, activeJobs)
					continue
				}

				if err := hetznerDeleteServer(cfg.APIToken, s.ID); err != nil {
					log.Printf("  ⚠️ delete server %s (%d) failed: %v", s.Name, s.ID, err)
					continue
				}
				log.Printf("  ✅ Deleted: %s (%d) — at billing boundary (%dm into hour)", s.Name, s.ID, minutesIntoHour)
				deleted++
			}

		default:
			log.Printf("🖥️  No scaling needed (needed=%d current=%d)", needed, len(current))
		}

		return nil
	}

	// ── 5. No pending — check billing boundary before deleting ────────
	if len(current) == 0 {
		idleTracker.Lock()
		idleTracker.since = nil
		idleTracker.Unlock()
		return nil
	}

	// Track when idle started (for idleMinutes minimum wait)
	idleTracker.Lock()
	if idleTracker.since == nil {
		now := time.Now()
		idleTracker.since = &now
		idleTracker.Unlock()
		log.Printf("🖥️  Hetzner idle detected — will delete servers near billing hour boundary")
		return nil
	}
	idleSince := *idleTracker.since
	idleTracker.Unlock()

	// Must be idle for at least idleMinutes before considering deletion
	if time.Since(idleSince) < time.Duration(cfg.IdleMinutes)*time.Minute {
		log.Printf("🖥️  Hetzner idle: %s / %s minimum idle before billing check",
			time.Since(idleSince).Round(time.Second),
			time.Duration(cfg.IdleMinutes)*time.Minute)
		return nil
	}

	// Delete servers that are within the deletion window of their billing hour
	deletedAny := false
	for _, s := range current {
		minutesIntoHour := int(time.Since(s.Created).Minutes()) % 60
		minutesUntilBoundary := 60 - minutesIntoHour
		inWindow := minutesIntoHour >= (60 - cfg.DeletionWindowMinutes)

		if !inWindow {
			log.Printf("🖥️  %s — idle but billing: %dm into hour, %dm until boundary (window starts at %dm)",
				s.Name, minutesIntoHour, minutesUntilBoundary, 60-cfg.DeletionWindowMinutes)
			continue
		}

		// Check active jobs before deleting
		activeJobs, err := database.VideoProcess().CountDocuments(ctx, bson.M{
			"workerId": bson.M{"$regex": "^" + s.Name + "@"},
			"status":   bson.M{"$in": bson.A{models.ProcessStatusPending, models.ProcessStatusProcessing}},
		})
		if err == nil && activeJobs > 0 {
			log.Printf("🖥️  %s — billing window (%dm) but %d job(s) still running → will retry at next hour boundary (~%dm)",
				s.Name, minutesIntoHour, activeJobs, 60-cfg.DeletionWindowMinutes)
			continue
		}

		log.Printf("🖥️  Deleting %s (idle + at billing boundary: %dm into hour)", s.Name, minutesIntoHour)
		if err := hetznerDeleteServer(cfg.APIToken, s.ID); err != nil {
			log.Printf("  ⚠️ delete %s failed: %v", s.Name, err)
			continue
		}
		log.Printf("  ✅ Deleted: %s", s.Name)
		deletedAny = true
	}

	if deletedAny {
		idleTracker.Lock()
		idleTracker.since = nil
		idleTracker.Unlock()
	}

	return nil
}

// ─── Hetzner API Helpers ──────────────────────────────────────────────

// hetznerListServers returns all servers with the managed-by label, sorted oldest first.
func hetznerListServers(token string) ([]hetznerServer, error) {
	url := hetznerAPIBase + "/servers?label_selector=" + strings.ReplaceAll(hetznerLabel, "=", "%3D")
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET servers: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET servers %d: %s", resp.StatusCode, string(body))
	}

	var result hetznerListResp
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode servers: %w", err)
	}

	// Sort oldest first
	servers := result.Servers
	for i := 0; i < len(servers)-1; i++ {
		for j := i + 1; j < len(servers); j++ {
			if servers[j].Created.Before(servers[i].Created) {
				servers[i], servers[j] = servers[j], servers[i]
			}
		}
	}

	return servers, nil
}

// HetznerListServersPublic is the exported version for the HTTP handler.
func HetznerListServersPublic(token string) ([]map[string]any, error) {
	servers, err := hetznerListServers(token)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, len(servers))
	for i, s := range servers {
		out[i] = map[string]any{
			"id":      s.ID,
			"name":    s.Name,
			"status":  s.Status,
			"ip":      s.IPv4(),
			"created": s.Created,
		}
	}
	return out, nil
}

// hetznerCreateServerNamed creates a Hetzner server with an explicit name.
func hetznerCreateServerNamed(cfg *HetznerAutoScaleConfig, name string) (string, error) {
	userData := fmt.Sprintf(
		"#!/bin/bash\nset -e\nunset NVM_DIR\nexport HOME=/root\ncurl -fsSL %s | bash -s -- --mongodb-uri %q --storage-path %s --count %d\n",
		cfg.InstallURL,
		cfg.MongoURI,
		cfg.StoragePath,
		cfg.DownloadsPerServer,
	)

	payload := hetznerCreateReq{
		Name:       name,
		ServerType: cfg.ServerType,
		Image:      "ubuntu-24.04",
		Location:   cfg.Location,
		UserData:   userData,
		SSHKeys:    cfg.SSHKeys,
		Labels: map[string]string{
			hetznerLabelKey: hetznerLabelVal,
		},
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", hetznerAPIBase+"/servers", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("POST server: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 {
		return "", fmt.Errorf("POST server %d: %s", resp.StatusCode, string(respBody))
	}

	// Extract public IPv4 from response for logging
	var createResp struct {
		Server struct {
			PublicNet struct {
				IPv4 struct {
					IP string `json:"ip"`
				} `json:"ipv4"`
			} `json:"public_net"`
		} `json:"server"`
	}
	if err := json.Unmarshal(respBody, &createResp); err == nil {
		ip := createResp.Server.PublicNet.IPv4.IP
		if ip != "" {
			log.Printf("  🌐 %s → %s (ssh root@%s)", name, ip, ip)
			log.Printf("  📋 Check: ssh root@%s 'cat /var/log/cloud-init-output.log'", ip)
		}
	}

	return name, nil
}

// hetznerDeleteServer deletes a Hetzner server by ID.
func hetznerDeleteServer(token string, serverID int64) error {
	url := fmt.Sprintf("%s/servers/%d", hetznerAPIBase, serverID)
	req, _ := http.NewRequest("DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("DELETE server: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 && resp.StatusCode != 204 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DELETE server %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// ─── Scheduler ────────────────────────────────────────────────────────

// StartHetznerScalerScheduler starts the auto-scaler goroutine.
// Config is read from DB on each tick, so changes take effect immediately.
func StartHetznerScalerScheduler(ctx context.Context) {
	log.Println("🖥️  Starting Hetzner scaler (every 1 min, config from DB: hetzner_auto_scale)...")

	runSync := func() {
		if err := SyncHetznerScaler(); err != nil {
			log.Printf("⚠️ Hetzner scaler failed: %v", err)
		}
	}

	runSync()

	now := time.Now()
	nextTick := now.Truncate(time.Minute).Add(time.Minute)
	select {
	case <-time.After(time.Until(nextTick)):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	runSync()

	for {
		select {
		case <-ctx.Done():
			log.Println("⏹️ Hetzner scaler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
