package services

import (
	"context"
	"log"
	"time"

	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── Space Capacity Sync ─────────────────────────────────────────────

// SyncSpaceCapacities fetches up to batch_capacity spaces (oldest-updated first),
// then for each space ID calculates total file size and updates FileCapacity.
// Batch size is read from settings (batch_capacity), default 5.
func SyncSpaceCapacities() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Read batch size from settings
	batchSize := GetBatchCapacity()

	// Fetch only batchSize spaces, sorted by capacity.updatedAt asc, createdAt asc
	// → null/missing updatedAt comes first (never synced), then oldest-synced first
	opts := options.Find().
		SetSort(bson.D{
			{Key: "capacity.lastUpdated", Value: 1},
			{Key: "createdAt", Value: 1},
		}).
		SetLimit(int64(batchSize))

	cursor, err := models.WorkspaceModel.Col().Find(ctx, bson.M{
		"metadata.deletedAt": bson.M{"$exists": false}, // exclude soft-deleted spaces
	}, opts)
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)

	var spaces []models.Workspace
	if err := cursor.All(ctx, &spaces); err != nil {
		return err
	}

	if len(spaces) == 0 {
		return nil
	}

	log.Printf("📦 Syncing capacity for %d space(s) (batch=%d)...", len(spaces), batchSize)

	for _, space := range spaces {
		if err := syncOneSpaceCapacity(ctx, &space); err != nil {
			log.Printf("⚠️ Failed to sync capacity for space %s: %v", space.ID, err)
		}
	}

	return nil
}

// syncOneSpaceCapacity calculates file usage for a single space and updates it.
// Sums metadata.size from files where spaceId == space.ID and type is not space/folder.
func syncOneSpaceCapacity(ctx context.Context, space *models.Workspace) error {
	// Single aggregate: sum files.metadata.size for all non-space/folder files in this space
	// Excludes files in trash (metadata.trashedAt) or soft-deleted (metadata.deletedAt)
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "spaceId", Value: space.ID},
			{Key: "type", Value: bson.D{{Key: "$nin", Value: bson.A{
				models.FileTypeSpace,
				models.FileTypeFolder,
			}}}},
			{Key: "metadata.size", Value: bson.D{
				{Key: "$exists", Value: true},
				{Key: "$gt", Value: 0},
			}},
			{Key: "metadata.trashedAt", Value: bson.D{{Key: "$exists", Value: false}}},
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: false}}},
		}}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "totalSize", Value: bson.D{{Key: "$sum", Value: "$metadata.size"}}},
		}}},
	}

	aggCursor, err := models.FileModel.Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}
	defer aggCursor.Close(ctx)

	var usedBytes int64 = 0
	type aggResult struct {
		TotalSize int64 `bson:"totalSize"`
	}
	if aggCursor.Next(ctx) {
		var res aggResult
		if err := aggCursor.Decode(&res); err == nil {
			usedBytes = res.TotalSize
		}
	}

	// Determine total capacity from plan.storageLimit
	// 0 or missing = unlimited (represented as nil)
	const TB int64 = 1024 * 1024 * 1024 * 1024
	var totalBytes *int64
	if space.Plan != nil && space.Plan.StorageLimit != nil {
		switch v := space.Plan.StorageLimit.(type) {
		case int64:
			if v > 0 {
				t := v * TB
				totalBytes = &t
			}
		case int32:
			if v > 0 {
				t := int64(v) * TB
				totalBytes = &t
			}
		case float64:
			if v > 0 {
				t := int64(v * float64(TB))
				totalBytes = &t
			}
		}
	}

	// Compute free and percentage
	var freeBytes *int64
	var percentage float64

	if totalBytes != nil && *totalBytes > 0 {
		f := *totalBytes - usedBytes
		if f < 0 {
			f = 0
		}
		freeBytes = &f
		percentage = float64(usedBytes) / float64(*totalBytes) * 100
		if percentage > 100 {
			percentage = 100
		}
	}

	// Build capacity document
	now := time.Now()
	capacity := bson.M{
		"used":        usedBytes,
		"percentage":  percentage,
		"lastUpdated": now,
	}
	if totalBytes != nil {
		capacity["total"] = *totalBytes
	} else {
		capacity["total"] = int64(0) // 0 = unlimited
	}
	if freeBytes != nil {
		capacity["free"] = *freeBytes
	} else {
		capacity["free"] = int64(0) // 0 = unlimited
	}

	// Update the space document
	// Use .Col() directly to bypass goose auto-inject updatedAt
	_, err = models.WorkspaceModel.Col().UpdateOne(ctx,
		bson.M{"_id": space.ID},
		bson.M{
			"$set": bson.M{
				"capacity": capacity,
			},
		},
	)
	if err != nil {
		return err
	}

	// Log result
	usedMB := float64(usedBytes) / 1024 / 1024
	if totalBytes != nil && *totalBytes > 0 {
		totalMB := float64(*totalBytes) / 1024 / 1024
		log.Printf("  ✅ [%s] \"%s\" (%s) used=%.2f MB / total=%.2f MB (%.2f%%)",
			space.ID[:8], space.Name, space.Slug, usedMB, totalMB, percentage)
	} else {
		log.Printf("  ✅ [%s] \"%s\" (%s) used=%.2f MB / total=unlimited",
			space.ID[:8], space.Name, space.Slug, usedMB)
	}

	return nil
}

// ─── Scheduler ────────────────────────────────────────────────────────

// StartSpaceCapacitySyncScheduler starts a background goroutine that syncs
// space capacities immediately, then waits until the next full minute and
// fires every 1 minute aligned to the wall clock.
func StartSpaceCapacitySyncScheduler(ctx context.Context) {
	log.Println("📦 Starting space capacity sync scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncSpaceCapacities(); err != nil {
			log.Printf("⚠️ Space capacity sync failed: %v", err)
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

	// Now tick every exact minute
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	runSync() // run at the first aligned tick

	for {
		select {
		case <-ctx.Done():
			log.Println("⏹️ Space capacity sync scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
