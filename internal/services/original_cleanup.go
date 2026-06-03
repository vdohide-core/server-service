package services

import (
	"context"
	"log"
	"strconv"
	"time"

	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ─── Original Cleanup (Soft-Delete Original Media) ────────────────────

// SyncOriginalCleanup finds media records with resolution "original" (type: video)
// where the file has already been transcoded to its highest possible resolution,
// then soft-deletes the original media by setting deletedAt.
//
// Logic:
//  1. Find medias: resolution="original", type="video", deletedAt not set
//  2. $lookup file → get metadata.highest (max resolution the source supports)
//  3. $lookup other medias for the same fileId (resolution≠"original", type="video")
//  4. If any transcoded media has resolution >= file.metadata.highest → soft-delete original
func SyncOriginalCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── 0. หา fileIds ที่กำลังประมวลผลอยู่ (ข้ามไฟล์เหล่านี้) ────
	processingFileIDs, _ := models.VideoProcessModel.Col().Distinct(ctx, "fileId", bson.M{
		"status": bson.M{"$in": bson.A{
			models.ProcessStatusPending,
			models.ProcessStatusProcessing,
		}},
	})

	// Aggregation pipeline on medias collection
	matchFilter := bson.D{
		{Key: "resolution", Value: models.ResolutionOriginal},
		{Key: "type", Value: models.MediaTypeVideo},
		{Key: "deletedAt", Value: bson.D{{Key: "$exists", Value: false}}},
		{Key: "fileId", Value: bson.D{{Key: "$exists", Value: true}}},
	}
	if len(processingFileIDs) > 0 {
		matchFilter = append(matchFilter, bson.E{Key: "fileId", Value: bson.D{{Key: "$nin", Value: processingFileIDs}}})
	}

	pipeline := mongo.Pipeline{
		// 1. หา original medias ที่ยังไม่ถูก soft-delete (ข้ามไฟล์ที่กำลัง process)
		{{Key: "$match", Value: matchFilter}},
		// 2. $lookup file → ดึง metadata.highest
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "files"},
			{Key: "localField", Value: "fileId"},
			{Key: "foreignField", Value: "_id"},
			{Key: "pipeline", Value: mongo.Pipeline{
				{{Key: "$match", Value: bson.D{
					{Key: "metadata.highest", Value: bson.D{{Key: "$exists", Value: true}}},
				}}},
				{{Key: "$project", Value: bson.D{
					{Key: "_id", Value: 1},
					{Key: "metadata.highest", Value: 1},
				}}},
			}},
			{Key: "as", Value: "_file"},
		}}},
		// 3. ต้องมี file ที่มี metadata.highest
		{{Key: "$match", Value: bson.D{
			{Key: "_file", Value: bson.D{{Key: "$ne", Value: bson.A{}}}},
		}}},
		// 4. $lookup medias อื่นๆ ของ fileId เดียวกัน (ที่ไม่ใช่ original, type=video, ยังไม่ลบ)
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "medias"},
			{Key: "localField", Value: "fileId"},
			{Key: "foreignField", Value: "fileId"},
			{Key: "pipeline", Value: mongo.Pipeline{
				{{Key: "$match", Value: bson.D{
					{Key: "resolution", Value: bson.D{{Key: "$ne", Value: models.ResolutionOriginal}}},
					{Key: "type", Value: models.MediaTypeVideo},
					{Key: "deletedAt", Value: bson.D{{Key: "$exists", Value: false}}},
				}}},
				{{Key: "$project", Value: bson.D{
					{Key: "resolution", Value: 1},
				}}},
			}},
			{Key: "as", Value: "_transcodedMedias"},
		}}},
		// 5. ต้องมี transcoded medias อย่างน้อย 1 ตัว
		{{Key: "$match", Value: bson.D{
			{Key: "_transcodedMedias.0", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		// 6. Project เฉพาะ fields ที่ต้องใช้
		{{Key: "$limit", Value: int64(1000)}},
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 1},
			{Key: "fileId", Value: 1},
			{Key: "_file", Value: 1},
			{Key: "_transcodedMedias", Value: 1},
		}}},
	}

	type fileProjection struct {
		Highest *int `bson:"metadata"`
	}

	type mediaProjection struct {
		Resolution *string `bson:"resolution"`
	}

	type result struct {
		ID               string `bson:"_id"`
		FileID           *string `bson:"fileId"`
		File             []bson.Raw `bson:"_file"`
		TranscodedMedias []bson.Raw `bson:"_transcodedMedias"`
	}

	cur, err := models.MediaModel.Aggregate(ctx, pipeline)
	if err != nil {
		return err
	}

	var results []result
	if err := cur.All(ctx, &results); err != nil {
		cur.Close(ctx)
		return err
	}
	cur.Close(ctx)

	if len(results) == 0 {
		return nil
	}

	// เช็คแต่ละ original media ว่ามี transcoded resolution >= highest หรือไม่
	var toDelete []string

	for _, r := range results {
		// ดึง highest จาก file
		if len(r.File) == 0 {
			continue
		}

		// Parse file document to get metadata.highest
		var fileDoc struct {
			Metadata struct {
				Highest *int `bson:"highest"`
			} `bson:"metadata"`
		}
		if err := bson.Unmarshal(r.File[0], &fileDoc); err != nil {
			continue
		}
		if fileDoc.Metadata.Highest == nil {
			continue
		}
		highest := *fileDoc.Metadata.Highest

		// เช็คว่ามี transcoded media ที่ resolution >= highest
		hasHighest := false
		for _, tmRaw := range r.TranscodedMedias {
			var tm struct {
				Resolution *string `bson:"resolution"`
			}
			if err := bson.Unmarshal(tmRaw, &tm); err != nil || tm.Resolution == nil {
				continue
			}

			// แปลง resolution string เป็น int เพื่อเทียบ
			res, err := strconv.Atoi(*tm.Resolution)
			if err != nil {
				continue // ข้าม non-numeric resolution (เช่น "trailer")
			}

			if res >= highest {
				hasHighest = true
				break
			}
		}

		if hasHighest {
			toDelete = append(toDelete, r.ID)
		}
	}

	if len(toDelete) == 0 {
		return nil
	}

	// Soft-delete original medias
	now := time.Now()
	res, err := models.MediaModel.Col().UpdateMany(ctx,
		bson.M{"_id": bson.M{"$in": toDelete}},
		bson.M{"$set": bson.M{"deletedAt": now}},
	)
	if err != nil {
		log.Printf("  ⚠️ original media soft-delete failed: %v", err)
		return err
	}

	log.Printf("🧹 Original cleanup: %d original media(s) soft-deleted (transcoded to highest)", res.ModifiedCount)
	return nil
}

// ─── Scheduler ────────────────────────────────────────────────────────

// StartOriginalCleanupScheduler starts a background goroutine that soft-deletes
// original media records after the file has been transcoded to its highest resolution.
// Runs immediately on startup, then every 1 minute wall-clock aligned.
func StartOriginalCleanupScheduler(ctx context.Context) {
	log.Println("🧹 Starting original cleanup scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncOriginalCleanup(); err != nil {
			log.Printf("⚠️ Original cleanup failed: %v", err)
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
			log.Println("⏹️ Original cleanup scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
