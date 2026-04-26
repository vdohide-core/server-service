package services

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"server-service/internal/db/database"
	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Domain Verification ─────────────────────────────────────────────

// SyncDomainVerifications fetches pending custom domains and verifies
// their CNAME records. Updates status to "active" if verified.
// Batch size is read from settings (batch_capacity), default 5.
func SyncDomainVerifications() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	batchSize := GetBatchCapacity()

	// Fetch only pending domains, oldest-verified (or never verified) first
	opts := options.Find().
		SetSort(bson.D{
			{Key: "dns.lastVerified", Value: 1},
			{Key: "createdAt", Value: 1},
		}).
		SetLimit(int64(batchSize))

	cursor, err := database.CustomDomains().Find(ctx, bson.M{
		"status": models.DomainStatusPending,
		"dns":    bson.M{"$exists": true},
	}, opts)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var domains []models.CustomDomain
	if err := cursor.All(ctx, &domains); err != nil {
		return err
	}

	if len(domains) == 0 {
		return nil
	}

	log.Printf("🌐 Verifying CNAME for %d pending domain(s) (batch=%d)...", len(domains), batchSize)

	for _, domain := range domains {
		verifyDomainHTTP(ctx, &domain)
	}

	return nil
}

// verifyDomainHTTP verifies a domain by calling its /health endpoint.
// Expects: {"status":"ok","service":"server-player"}
func verifyDomainHTTP(ctx context.Context, domain *models.CustomDomain) {
	url := "http://" + domain.Name + "/health"

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("  ❌ [%s] \"%s\" — request error: %v", domain.ID[:8], domain.Name, err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("  ❌ [%s] \"%s\" — unreachable: %v", domain.ID[:8], domain.Name, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("  ⏳ [%s] \"%s\" — HTTP %d (want 200)", domain.ID[:8], domain.Name, resp.StatusCode)
		return
	}

	var body struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		log.Printf("  ⏳ [%s] \"%s\" — invalid JSON response", domain.ID[:8], domain.Name)
		return
	}

	now := time.Now()

	if body.Status == "ok" && body.Service == "server-player" {
		// Verified — mark active
		_, err = database.CustomDomains().UpdateOne(ctx,
			bson.M{"_id": domain.ID},
			bson.M{"$set": bson.M{
				"status":           models.DomainStatusActive,
				"dns.lastVerified": now,
				"updatedAt":        now,
			}},
		)
		if err != nil {
			log.Printf("  ⚠️ [%s] \"%s\" — failed to update status: %v", domain.ID[:8], domain.Name, err)
			return
		}
		log.Printf("  ✅ [%s] \"%s\" → active", domain.ID[:8], domain.Name)
	} else {
		// Wrong service — update lastVerified to rotate to back of queue
		_, _ = database.CustomDomains().UpdateOne(ctx,
			bson.M{"_id": domain.ID},
			bson.M{"$set": bson.M{
				"dns.lastVerified": now,
				"updatedAt":        now,
			}},
		)
		log.Printf("  ⏳ [%s] \"%s\" — wrong service (got: status=%s service=%s)",
			domain.ID[:8], domain.Name, body.Status, body.Service)
	}
}

// ─── Scheduler ───────────────────────────────────────────────────────

// StartDomainVerifyScheduler starts a background goroutine that verifies
// pending domain CNAMEs immediately, then fires every 1 minute aligned
// to the wall clock (:00 of each minute, crontab-style).
func StartDomainVerifyScheduler(ctx context.Context) {
	log.Println("🌐 Starting domain CNAME verify scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncDomainVerifications(); err != nil {
			log.Printf("⚠️ Domain verify failed: %v", err)
		}
	}

	// Run immediately on startup
	runSync()

	// Wait until the next full minute boundary
	now := time.Now()
	nextTick := now.Truncate(time.Minute).Add(time.Minute)
	select {
	case <-time.After(time.Until(nextTick)):
	case <-ctx.Done():
		return
	}

	// Now tick every exact minute (crontab: * * * * *)
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	runSync() // run at the first aligned tick

	for {
		select {
		case <-ctx.Done():
			log.Println("⏹️ Domain verify scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
