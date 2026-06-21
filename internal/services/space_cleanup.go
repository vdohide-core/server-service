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

// ─── Space Cleanup ────────────────────────────────────────────────────

const (
	// ระยะเวลารอหลังลบ space ก่อน mark ไฟล์ (เฉพาะ hobby plan)
	spaceCleanupDelay = 10 * time.Minute
)

// SyncSpaceCleanup หา workspace ที่ถูก soft-delete แล้ว mark ไฟล์เพื่อลบ
// ตาม plan type:
//   - hobby:  รอ 10 นาทีหลังลบ → mark ไฟล์ทั้งหมด
//   - อื่นๆ:  รอจนกว่า plan.expiresAt หมดอายุ → mark ไฟล์
func SyncSpaceCleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	now := time.Now()

	// ── 1. Hobby plan: ลบไปแล้ว > 10 นาที ───────────────────────────
	hobbyCutoff := now.Add(-spaceCleanupDelay)
	hobbyFilter := bson.M{
		"metadata.deletedAt": bson.M{
			"$exists": true,
			"$lte":    hobbyCutoff,
		},
		"$or": bson.A{
			bson.M{"plan": bson.M{"$exists": false}},
			bson.M{"plan.planType": models.PlanTypeHobby},
		},
	}

	// ── 2. Paid plans: expiresAt หมดอายุแล้ว ─────────────────────────
	paidFilter := bson.M{
		"metadata.deletedAt": bson.M{"$exists": true},
		"plan.planType":      bson.M{"$ne": models.PlanTypeHobby},
		"plan.expiresAt":     bson.M{"$exists": true, "$lte": now},
	}

	// รวม filter ทั้งสอง
	combinedFilter := bson.M{
		"$or": bson.A{hobbyFilter, paidFilter},
	}

	cursor, err := models.WorkspaceModel.Col().Find(ctx, combinedFilter,
		options.Find().SetLimit(100).SetProjection(bson.M{
			"_id":      1,
			"name":     1,
			"slug":     1,
			"metadata": 1,
			"plan":     1,
		}),
	)
	if err != nil {
		return err
	}

	var spaces []models.Workspace
	if err := cursor.All(ctx, &spaces); err != nil {
		cursor.Close(ctx)
		return err
	}
	cursor.Close(ctx)

	if len(spaces) == 0 {
		return nil
	}

	log.Printf("🧹 Space cleanup: %d space(s) ready for file marking...", len(spaces))

	for _, space := range spaces {
		markSpaceFilesForDeletion(ctx, &space, now)
	}

	// ── 3. ลบ workspace ที่ไม่มีไฟล์เหลือแล้ว ─────────────────────
	hardDeleteEmptyWorkspaces(ctx)

	return nil
}

// hardDeleteEmptyWorkspaces หา workspace ที่ถูก soft-delete
// และไม่มีไฟล์เหลืออยู่แล้ว → ลบ workspace + ข้อมูลที่เกี่ยวข้องทั้งหมด
func hardDeleteEmptyWorkspaces(ctx context.Context) {
	pipeline := mongo.Pipeline{
		// หา workspace ที่ถูก soft-delete
		{{Key: "$match", Value: bson.D{
			{Key: "metadata.deletedAt", Value: bson.D{{Key: "$exists", Value: true}}},
		}}},
		// เช็คว่ายังมีไฟล์เหลือไหม ($lookup จาก files)
		{{Key: "$lookup", Value: bson.D{
			{Key: "from", Value: "files"},
			{Key: "localField", Value: "_id"},
			{Key: "foreignField", Value: "spaceId"},
			{Key: "pipeline", Value: mongo.Pipeline{
				{{Key: "$limit", Value: 1}}, // แค่เช็คว่ามีหรือไม่
			}},
			{Key: "as", Value: "_files"},
		}}},
		// เอาเฉพาะที่ไม่มีไฟล์เหลือ
		{{Key: "$match", Value: bson.D{
			{Key: "_files", Value: bson.D{{Key: "$size", Value: 0}}},
		}}},
		{{Key: "$limit", Value: 100}},
		{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 1},
			{Key: "name", Value: 1},
		}}},
	}

	type wsTarget struct {
		ID   string `bson:"_id"`
		Name string `bson:"name"`
	}

	cur, err := models.WorkspaceModel.Aggregate(ctx, pipeline)
	if err != nil {
		log.Printf("  ⚠️ workspace hard-delete pipeline failed: %v", err)
		return
	}

	var targets []wsTarget
	if err := cur.All(ctx, &targets); err != nil {
		cur.Close(ctx)
		return
	}
	cur.Close(ctx)

	if len(targets) == 0 {
		return
	}

	wsIDs := make([]string, len(targets))
	for i, w := range targets {
		wsIDs[i] = w.ID
	}

	// ลบข้อมูลที่เกี่ยวข้องทั้งหมด (cascade)
	spaceFilter := bson.M{"spaceId": bson.M{"$in": wsIDs}}
	members, _ := models.WorkspaceMemberModel.DeleteMany(ctx, spaceFilter)
	domains, _ := models.CustomDomainModel.DeleteMany(ctx, spaceFilter)
	oauths, _ := models.OAuthModel.DeleteMany(ctx, spaceFilter)
	apiKeys, _ := models.ApiKeyModel.DeleteMany(ctx, spaceFilter)

	// ลบ workspace เอง
	wsRes, _ := models.WorkspaceModel.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": wsIDs}})
	wsDeleted := int64(0)
	if wsRes != nil {
		wsDeleted = wsRes.DeletedCount
	}

	log.Printf("  ✅ %d workspace(s) hard deleted — members=%d domains=%d oauths=%d apiKeys=%d",
		wsDeleted, members.DeletedCount, domains.DeletedCount,
		oauths.DeletedCount, apiKeys.DeletedCount,
	)
}

// markSpaceFilesForDeletion mark ไฟล์ทั้งหมดใน space ให้พร้อมลบ
// ตั้ง metadata.trashedAt, trashedBy, deletedAt, deletedBy
// เฉพาะไฟล์ที่ยังไม่มี metadata.deletedAt (ป้องกัน mark ซ้ำ)
// รองรับ 100K+ ไฟล์ — ใช้ UpdateMany ตรง ไม่ต้องโหลด ID ขึ้น memory
func markSpaceFilesForDeletion(ctx context.Context, space *models.Workspace, now time.Time) {
	deletedBy := ""
	if space.Metadata != nil && space.Metadata.DeletedBy != nil {
		deletedBy = *space.Metadata.DeletedBy
	}

	planType := models.PlanTypeHobby
	if space.Plan != nil {
		planType = space.Plan.PlanType
	}

	// ── 1. หา fileIds ที่กำลังประมวลผลอยู่ (ข้ามไฟล์เหล่านี้) ────
	processingFileIDs, _ := models.VideoProcessModel.Col().Distinct(ctx, "fileId", bson.M{
		"status": bson.M{"$in": bson.A{
			models.ProcessStatusPending,
			models.ProcessStatusProcessing,
		}},
	})

	// ── 2. Mark ไฟล์ตรงด้วย spaceId (ข้ามไฟล์ที่กำลังประมวลผล) ──
	fileFilter := bson.M{
		"spaceId":            space.ID,
		"metadata.deletedAt": bson.M{"$exists": false},
	}
	if len(processingFileIDs) > 0 {
		fileFilter["_id"] = bson.M{"$nin": processingFileIDs}
	}

	res, err := models.FileModel.Col().UpdateMany(ctx, fileFilter, bson.M{
		"$set": bson.M{
			"metadata.trashedAt": now,
			"metadata.trashedBy": deletedBy,
			"metadata.deletedAt": now,
			"metadata.deletedBy": deletedBy,
			"updatedAt":          now,
		},
	})
	if err != nil {
		log.Printf("  ⚠️ [%s] \"%s\" — file marking failed: %v", space.ID[:8], space.Name, err)
		return
	}

	if res.ModifiedCount == 0 {
		return
	}

	// ── 2. Mark medias — ดึง fileIds ด้วย Distinct (เบา) ─────────
	fileIDs, err := models.FileModel.Col().Distinct(ctx, "_id", bson.M{"spaceId": space.ID})
	if err != nil {
		log.Printf("  ⚠️ [%s] \"%s\" — distinct fileIds failed: %v", space.ID[:8], space.Name, err)
		return
	}

	mediasDeleted := int64(0)
	if len(fileIDs) > 0 {
		mediaRes, _ := models.MediaModel.Col().UpdateMany(ctx,
			bson.M{
				"fileId":    bson.M{"$in": fileIDs},
				"deletedAt": bson.M{"$exists": false},
			},
			bson.M{"$set": bson.M{"deletedAt": now}},
		)
		if mediaRes != nil {
			mediasDeleted = mediaRes.ModifiedCount
		}
	}

	log.Printf("  ✅ [%s] \"%s\" (%s) — %d file(s) + %d media(s) marked for deletion (plan=%s)",
		space.ID[:8], space.Name, space.Slug, res.ModifiedCount, mediasDeleted, planType)
}

// ─── Scheduler ────────────────────────────────────────────────────────

// StartSpaceCleanupScheduler เริ่ม goroutine สำหรับ:
// 1. Mark ไฟล์ใน space ที่ถูกลบ
// 2. ลบ workspace ที่ไม่มีไฟล์เหลือ
// ทำงานทุก 1 นาที (wall-clock aligned)
func StartSpaceCleanupScheduler(ctx context.Context) {
	log.Println("🧹 Starting space cleanup scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncSpaceCleanup(); err != nil {
			log.Printf("⚠️ Space cleanup failed: %v", err)
		}
	}

	// รันทันทีตอนเริ่ม
	runSync()

	// รอจนถึงนาทีถัดไป (wall-clock aligned)
	now := time.Now()
	nextTick := now.Truncate(time.Minute).Add(time.Minute)
	select {
	case <-time.After(time.Until(nextTick)):
	case <-ctx.Done():
		return
	}

	// ทำงานทุก 1 นาที
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	runSync()

	for {
		select {
		case <-ctx.Done():
			log.Println("⏹️ Space cleanup scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
