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

// ─── File Cleanup (Hard Delete) ───────────────────────────────────────

// SyncFileCleanup finds soft-deleted files (metadata.deletedAt exists)
// that are safe to hard delete, using aggregation $lookup per type:
//   - space:  no files with spaceId == file.ID
//   - folder: no files with parentId == file.ID
//   - others: no medias with fileId == file.ID
func SyncFileCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fixed limit: process up to 1000 ready-to-delete files per type per run
	var limit int64 = 1000

	type cleanTarget struct {
		ID   string `bson:"_id"`
		Name string `bson:"name"`
		Type string `bson:"type"`
	}

	var targets []cleanTarget

	// ── 1. Folders: no files left with parentId == _id ───────────────
	folderPipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "type", Value: models.FileTypeFolder},
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "files"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "parentId"},
			{Key: "as", Value: "_children"},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "_children", Value: bson.D{{Key: "$size", Value: 0}}},
		}}},
		{{Key: "$limit", Value: limit}},
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 1},
			{Key: "name", Value: 1},
			{Key: "type", Value: 1},
		}}},
	}

	// ── 2. Other files: no media AND no ingest records with fileId == _id
	filePipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "type", Value: bson.D{{Key: "$nin", Value: bson.A{models.FileTypeSpace, models.FileTypeFolder}}}},
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "medias"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_medias"},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "ingests"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_ingests"},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "video_process"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "as", Value: "_videoProcess"},
		}}},
		{{Key: "$match", Value: bson.D{
			{Key: "_medias", Value: bson.D{{Key: "$size", Value: 0}}},
			{Key: "_ingests", Value: bson.D{{Key: "$size", Value: 0}}},
			{Key: "_videoProcess", Value: bson.D{{Key: "$size", Value: 0}}},
		}}},
		{{Key: "$limit", Value: limit}},
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 1},
			{Key: "name", Value: 1},
			{Key: "type", Value: 1},
		}}},
	}

	// Run folder + file aggregations
	for _, pipeline := range []mongo.Pipeline{folderPipeline, filePipeline} {
		cur, err := models.FileModel.Aggregate(ctx, pipeline)
		if err != nil {
			return err
		}
		var batch []cleanTarget
		if err := cur.All(ctx, &batch); err != nil {
			cur.Close(ctx)
			return err
		}
		cur.Close(ctx)
		targets = append(targets, batch...)
	}

	if len(targets) == 0 {
		return nil
	}

	log.Printf("🗑️  Hard deleting %d file(s) ready for cleanup...", len(targets))

	deleted := int64(0)

	// Collect all target IDs for starred cleanup
	allIDs := make([]string, 0, len(targets))
	for _, t := range targets {
		allIDs = append(allIDs, t.ID)
	}

	// ── Starred: delete all starreds referencing deleted files ────────
	starreds, _ := models.StarredModel.DeleteMany(ctx, bson.M{"fileId": bson.M{"$in": allIDs}})

	// ── Files: single DeleteMany ────────────────────────────────────
	res, err := models.FileModel.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": allIDs}})
	if err != nil {
		log.Printf("  ⚠️ file bulk delete failed: %v", err)
	} else {
		deleted += res.DeletedCount
		log.Printf("  ✅ %d file(s) hard deleted", res.DeletedCount)
	}
	// ── Orphaned starreds: fileId → check via _id index (fast) ──────
	allFileIDs, err := models.StarredModel.Col().Distinct(ctx, "fileId", bson.M{})
	if err == nil && len(allFileIDs) > 0 {
		fileIDStrs := make([]string, 0, len(allFileIDs))
		for _, id := range allFileIDs {
			if s, ok := id.(string); ok {
				fileIDStrs = append(fileIDStrs, s)
			}
		}

		cur, err := models.FileModel.Col().Find(ctx, bson.M{"_id": bson.M{"$in": fileIDStrs}}, options.Find().SetProjection(bson.M{"_id": 1}))
		if err == nil {
			existingIDs := make(map[string]bool)
			for cur.Next(ctx) {
				var doc struct{ ID string `bson:"_id"` }
				if cur.Decode(&doc) == nil {
					existingIDs[doc.ID] = true
				}
			}
			cur.Close(ctx)

			var orphanedFileIDs []string
			for _, id := range fileIDStrs {
				if !existingIDs[id] {
					orphanedFileIDs = append(orphanedFileIDs, id)
				}
			}

			if len(orphanedFileIDs) > 0 {
				if res, err := models.StarredModel.DeleteMany(ctx, bson.M{"fileId": bson.M{"$in": orphanedFileIDs}}); err == nil {
					starreds.DeletedCount += res.DeletedCount
				}
			}
		}
	}

	log.Printf("🗑️  Done: %d hard deleted, starreds=%d", deleted, starreds.DeletedCount)
	return nil
}

// ─── Scheduler ────────────────────────────────────────────────────────

// StartFileCleanupScheduler starts a background goroutine that hard-deletes
// soft-deleted files immediately on startup, then every 1 minute wall-clock aligned.
func StartFileCleanupScheduler(ctx context.Context) {
	log.Println("🗑️  Starting file cleanup scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncFileCleanup(); err != nil {
			log.Printf("⚠️ File cleanup failed: %v", err)
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

	// Tick every exact minute (crontab: * * * * *)
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	runSync() // run at the first aligned tick

	for {
		select {
		case <-ctx.Done():
			log.Println("⏹️ File cleanup scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
