package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Domain Verification ─────────────────────────────────────────────

const (
	// domainVerifyCooldown is the minimum wait after the first verify attempt.
	domainVerifyCooldown = 5 * time.Minute
	// domainMaxRetries is the max retry count before marking failed.
	domainMaxRetries = 3
)

// SyncDomainVerifications fetches pending custom domains and verifies
// their CNAME records. Updates status to "active" if verified.
// Skips domains that were verified less than 5 minutes ago.
// After 3 failed retries, marks the domain as "failed".
func SyncDomainVerifications() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	batchSize := GetBatchCapacity()

	// Fetch only pending domains, oldest-verified (or never verified) first
	// Skip domains that were verified less than 5 minutes ago
	cooldownCutoff := time.Now().Add(-domainVerifyCooldown)
	opts := options.Find().
		SetSort(bson.D{
			{Key: "dns.lastVerified", Value: 1},
			{Key: "createdAt", Value: 1},
		}).
		SetLimit(int64(batchSize))

	cursor, err := models.CustomDomainModel.Col().Find(ctx, bson.M{
		"status": models.DomainStatusPending,
		"dns":    bson.M{"$exists": true},
		"$or": bson.A{
			bson.M{"dns.lastVerified": bson.M{"$exists": false}},
			bson.M{"dns.lastVerified": nil},
			bson.M{"dns.lastVerified": bson.M{"$lte": cooldownCutoff}},
		},
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
// On failure: increments retryCount. After 3 retries → status="failed" with reason.
func verifyDomainHTTP(ctx context.Context, domain *models.CustomDomain) {
	url := "http://" + domain.Name + "/health"
	now := time.Now()

	retryCount := 0
	if domain.DNS != nil {
		retryCount = domain.DNS.RetryCount
	}

	// Helper: mark domain as failed
	markFailed := func(reason string) {
		_, _ = models.CustomDomainModel.UpdateOne(ctx,
			bson.M{"_id": domain.ID},
			bson.M{"$set": bson.M{
				"status":           models.DomainStatusFailed,
				"dns.retryCount":   retryCount + 1,
				"dns.lastVerified": now,
				"dns.reason":       reason,
				"updatedAt":        now,
			}},
		)
		log.Printf("  ❌ [%s] \"%s\" → failed (%s)", domain.ID[:8], domain.Name, reason)
	}

	// Helper: increment retry count
	incrementRetry := func(reason string) {
		newCount := retryCount + 1
		if newCount >= domainMaxRetries {
			markFailed(reason)
			return
		}
		_, _ = models.CustomDomainModel.UpdateOne(ctx,
			bson.M{"_id": domain.ID},
			bson.M{"$set": bson.M{
				"dns.retryCount":   newCount,
				"dns.lastVerified": now,
				"dns.reason":       reason,
				"updatedAt":        now,
			}},
		)
		log.Printf("  ⏳ [%s] \"%s\" — retry %d/%d (%s)",
			domain.ID[:8], domain.Name, newCount, domainMaxRetries, reason)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		incrementRetry("request error: " + err.Error())
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		incrementRetry("unreachable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		incrementRetry(fmt.Sprintf("HTTP %d (want 200)", resp.StatusCode))
		return
	}

	var body struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		incrementRetry("invalid JSON response")
		return
	}

	if body.Status == "ok" && body.Service == "server-player" {
		// Verified — mark active, reset retryCount
		_, err = models.CustomDomainModel.UpdateOne(ctx,
			bson.M{"_id": domain.ID},
			bson.M{
				"$set": bson.M{
					"status":           models.DomainStatusActive,
					"dns.retryCount":   0,
					"dns.lastVerified": now,
					"updatedAt":        now,
				},
				"$unset": bson.M{
					"dns.reason": "",
				},
			},
		)
		if err != nil {
			log.Printf("  ⚠️ [%s] \"%s\" — failed to update status: %v", domain.ID[:8], domain.Name, err)
			return
		}
		log.Printf("  ✅ [%s] \"%s\" → active", domain.ID[:8], domain.Name)
	} else {
		incrementRetry(fmt.Sprintf("wrong service (status=%s service=%s)", body.Status, body.Service))
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
