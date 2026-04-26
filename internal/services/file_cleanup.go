package services

import (
	"context"
	"log"
	"time"

	"server-service/internal/db/database"
	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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

	// ── 1. Spaces: no files left with spaceId == _id ────────────────
	spacePipeline := mongo.Pipeline{
		{{Key: "$match", Value: bson.D{
			{Key: "type", Value: models.FileTypeSpace},
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "files"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "spaceId"},
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

	// ── 2. Folders: no files left with parentId == _id ───────────────
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

	// ── 3. Other files: no media AND no ingest records with fileId == _id
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

	// Run all 3 aggregations
	for _, pipeline := range []mongo.Pipeline{spacePipeline, folderPipeline, filePipeline} {
		cur, err := database.Files().Aggregate(ctx, pipeline)
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

	// Split by type
	var spaceIDs, otherIDs []string

	for _, t := range targets {
		if t.Type == models.FileTypeSpace {
			spaceIDs = append(spaceIDs, t.ID)
		} else {
			otherIDs = append(otherIDs, t.ID)
		}
	}

	deleted := int64(0)

	// ── Spaces: cascade DeleteMany then delete files ─────────────────
	if len(spaceIDs) > 0 {
		spaceFilter := bson.M{"spaceId": bson.M{"$in": spaceIDs}}

		members, _ := database.WorkspaceMembers().DeleteMany(ctx, spaceFilter)
		domains, _ := database.CustomDomains().DeleteMany(ctx, spaceFilter)
		oauths, _  := database.Oauths().DeleteMany(ctx, spaceFilter)
		apiKeys, _ := database.ApiKeys().DeleteMany(ctx, spaceFilter)

		res, err := database.Files().DeleteMany(ctx, bson.M{"_id": bson.M{"$in": spaceIDs}})
		if err != nil {
			log.Printf("  ⚠️ space bulk delete failed: %v", err)
		} else {
			deleted += res.DeletedCount
			log.Printf("  ✅ %d space(s) hard deleted — members=%d domains=%d oauths=%d apiKeys=%d",
				res.DeletedCount, members.DeletedCount, domains.DeletedCount,
				oauths.DeletedCount, apiKeys.DeletedCount,
			)
		}
	}

	// ── Other files: single DeleteMany ──────────────────────────────
	if len(otherIDs) > 0 {
		res, err := database.Files().DeleteMany(ctx, bson.M{"_id": bson.M{"$in": otherIDs}})
		if err != nil {
			log.Printf("  ⚠️ file bulk delete failed: %v", err)
		} else {
			deleted += res.DeletedCount
			log.Printf("  ✅ %d file(s) hard deleted", res.DeletedCount)
		}
	}

	log.Printf("🗑️  Done: %d hard deleted", deleted)
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
