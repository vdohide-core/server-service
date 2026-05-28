package services

import (
	"context"
	"log"
	"time"

	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
)

const (
	workerStaleTimeout  = 3 * time.Minute  // mark offline after 3 min without heartbeat
	workerDeleteTimeout = 1 * time.Hour    // delete worker record after 1 hour offline
)

// SyncWorkerCleanup marks stale workers as offline and deletes very old ones.
func SyncWorkerCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	now := time.Now()

	// ── 1. Mark workers offline if heartbeat is stale ─────────────
	staleCutoff := now.Add(-workerStaleTimeout)
	markResult, err := models.WorkerModel.Col().UpdateMany(ctx,
		bson.M{
			"status":      bson.M{"$ne": models.WorkerStatusOffline},
			"heartbeatAt": bson.M{"$lt": staleCutoff},
		},
		bson.M{"$set": bson.M{
			"status":    models.WorkerStatusOffline,
			"updatedAt": now,
		}},
	)
	if err != nil {
		return err
	}

	// ── 2. Delete workers that have been offline for too long ─────
	deleteCutoff := now.Add(-workerDeleteTimeout)
	delResult, err := models.WorkerModel.Col().DeleteMany(ctx,
		bson.M{
			"status":      models.WorkerStatusOffline,
			"heartbeatAt": bson.M{"$lt": deleteCutoff},
		},
	)
	if err != nil {
		return err
	}

	if markResult.ModifiedCount > 0 || delResult.DeletedCount > 0 {
		log.Printf("🧹 Workers: marked %d offline, deleted %d stale",
			markResult.ModifiedCount, delResult.DeletedCount)
	}

	return nil
}

// StartWorkerCleanupScheduler runs worker cleanup every 1 minute.
func StartWorkerCleanupScheduler(ctx context.Context) {
	log.Println("👷 Starting worker cleanup scheduler (every 1 min)...")

	runSync := func() {
		if err := SyncWorkerCleanup(); err != nil {
			log.Printf("⚠️ Worker cleanup failed: %v", err)
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
			log.Println("⏹️ Worker cleanup scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
