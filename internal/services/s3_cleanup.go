package services

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"server-service/internal/db/database"
	"server-service/internal/db/models"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── S3 Storage Cleanup ───────────────────────────────────────────────

// s3Record is a lightweight projection for media/ingest records.
type s3Record struct {
	ID        string  `bson:"_id"`
	StorageID *string `bson:"storageId"`
	Path      *string `bson:"path"`
}

// SyncS3Cleanup finds soft-deleted files (metadata.deletedAt exists),
// then deletes their associated media and ingest objects from S3,
// followed by removing those DB records.
// This runs separately from file_cleanup.go (which deletes the File documents themselves).
func SyncS3Cleanup() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// ── 1. Find IDs of all soft-deleted files ─────────────────────────
	cur, err := database.Files().Find(ctx,
		bson.M{"metadata.deletedAt": bson.M{"$exists": true}},
		options.Find().SetProjection(bson.M{"_id": 1}),
	)
	if err != nil {
		return fmt.Errorf("find deleted files: %w", err)
	}

	type onlyID struct {
		ID string `bson:"_id"`
	}
	var fileDocs []onlyID
	if err := cur.All(ctx, &fileDocs); err != nil {
		return fmt.Errorf("decode file IDs: %w", err)
	}
	cur.Close(ctx)

	if len(fileDocs) == 0 {
		return nil
	}

	fileIDs := make([]string, len(fileDocs))
	for i, f := range fileDocs {
		fileIDs[i] = f.ID
	}

	// ── 2. Load all S3 storages (cached per run) ──────────────────────
	storageMap, err := loadS3StorageMap(ctx)
	if err != nil {
		return fmt.Errorf("load storages: %w", err)
	}

	log.Printf("🪣  S3 cleanup: %d deleted file(s), %d S3 storage(s) found", len(fileIDs), len(storageMap))

	// ── 3. Purge medias ───────────────────────────────────────────────
	mDel, err := purgeS3Collection(ctx, database.Medias(), fileIDs, storageMap, "media")
	if err != nil {
		log.Printf("  ⚠️ media S3 purge error: %v", err)
	}

	// ── 4. Purge ingests ──────────────────────────────────────────────
	iDel, err := purgeS3Collection(ctx, database.Ingests(), fileIDs, storageMap, "ingest")
	if err != nil {
		log.Printf("  ⚠️ ingest S3 purge error: %v", err)
	}

	if mDel+iDel > 0 {
		log.Printf("🪣  S3 cleanup done: media=%d ingest=%d records purged", mDel, iDel)
	}

	return nil
}

// purgeS3Collection fetches records from col where fileId is in fileIDs,
// deletes S3 objects, then bulk-deletes the DB records.
// Returns number of DB records deleted.
func purgeS3Collection(
	ctx context.Context,
	col *mongo.Collection,
	fileIDs []string,
	storageMap map[string]*models.Storage,
	label string,
) (int64, error) {
	cur, err := col.Find(ctx,
		bson.M{"fileId": bson.M{"$in": fileIDs}},
		options.Find().SetProjection(bson.M{"_id": 1, "storageId": 1, "path": 1}),
	)
	if err != nil {
		return 0, fmt.Errorf("find %s records: %w", label, err)
	}
	defer cur.Close(ctx)

	var records []s3Record
	if err := cur.All(ctx, &records); err != nil {
		return 0, fmt.Errorf("decode %s records: %w", label, err)
	}

	if len(records) == 0 {
		return 0, nil
	}

	// Delete S3 objects first — only collect IDs of records that belong to
	// an S3 storage so we don't accidentally purge local-storage media records.
	s3Deleted := 0
	var docIDs []string
	for _, rec := range records {
		if rec.StorageID == nil || rec.Path == nil || *rec.Path == "" {
			continue
		}
		storage, ok := storageMap[*rec.StorageID]
		if !ok {
			continue // local or unknown storage — skip entirely
		}

		// This record belongs to an S3 storage, mark it for DB deletion
		docIDs = append(docIDs, rec.ID)

		if err := deleteS3Object(ctx, storage, *rec.Path); err != nil {
			log.Printf("  ⚠️ S3 delete failed [%s] %s: %v", label, rec.ID[:8], err)
			continue
		}
		s3Deleted++
	}

	// Nothing to purge from DB — all records belong to local/unknown storage
	if len(docIDs) == 0 {
		return 0, nil
	}

	// Bulk-delete DB records
	res, err := col.DeleteMany(ctx, bson.M{"_id": bson.M{"$in": docIDs}})
	if err != nil {
		return 0, fmt.Errorf("delete %s DB records: %w", label, err)
	}

	log.Printf("  ✅ %s: %d S3 object(s) deleted, %d DB record(s) removed",
		label, s3Deleted, res.DeletedCount)

	return res.DeletedCount, nil
}

// ─── S3 Helpers ───────────────────────────────────────────────────────

// loadS3StorageMap returns all S3-type storages keyed by their ID.
func loadS3StorageMap(ctx context.Context) (map[string]*models.Storage, error) {
	cur, err := database.Storages().Find(ctx, bson.M{"type": "s3"})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var storages []models.Storage
	if err := cur.All(ctx, &storages); err != nil {
		return nil, err
	}

	m := make(map[string]*models.Storage, len(storages))
	for i := range storages {
		m[storages[i].ID] = &storages[i]
	}
	return m, nil
}

// deleteS3Object deletes a file from S3-compatible storage.
// For versioned buckets (e.g. Backblaze B2), it lists and deletes all versions
// and delete markers so no hidden copies remain.
func deleteS3Object(ctx context.Context, storage *models.Storage, objectPath string) error {
	if storage.S3 == nil {
		return fmt.Errorf("storage has no S3 config")
	}
	cfg := storage.S3

	endpoint := strings.TrimRight(*cfg.Endpoint, "/")
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}
	if strings.HasSuffix(endpoint, "/"+cfg.Bucket) {
		endpoint = endpoint[:len(endpoint)-len(cfg.Bucket)-1]
	}

	objectKey := objectPath
	if cfg.Prefix != "" && !strings.HasPrefix(objectPath, cfg.Prefix) {
		objectKey = strings.TrimRight(cfg.Prefix, "/") + "/" + objectPath
	}

	region := cfg.Region
	if region == "" {
		region = "auto"
	}

	client := s3.New(s3.Options{
		Region:       region,
		BaseEndpoint: &endpoint,
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID,
			cfg.SecretAccessKey,
			"",
		),
		UsePathStyle: cfg.ForcePathStyle,
	})

	bucket := aws.String(cfg.Bucket)
	key := aws.String(objectKey)

	// ── Try to list all versions (handles B2 versioned buckets) ──────
	versionsOut, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
		Bucket: bucket,
		Prefix: key,
	})
	if err == nil && versionsOut != nil && (len(versionsOut.Versions) > 0 || len(versionsOut.DeleteMarkers) > 0) {
		// Delete each version + delete marker by VersionId
		for _, v := range versionsOut.Versions {
			_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket:    bucket,
				Key:       v.Key,
				VersionId: v.VersionId,
			})
			if err != nil {
				return fmt.Errorf("DeleteObject version %s: %w", aws.ToString(v.VersionId), err)
			}
		}
		for _, dm := range versionsOut.DeleteMarkers {
			_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
				Bucket:    bucket,
				Key:       dm.Key,
				VersionId: dm.VersionId,
			})
			if err != nil {
				return fmt.Errorf("DeleteObject marker %s: %w", aws.ToString(dm.VersionId), err)
			}
		}
		return nil
	}

	// ── Fallback: simple delete (R2, MinIO, non-versioned) ───────────
	_, err = client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: bucket,
		Key:    key,
	})
	if err != nil {
		return fmt.Errorf("DeleteObject %s: %w", objectKey, err)
	}
	return nil
}


// ─── Scheduler ────────────────────────────────────────────────────────

// StartS3CleanupScheduler starts a background goroutine that purges S3 media/ingest
// objects for soft-deleted files. Runs immediately, then every 1 minute wall-clock aligned.
func StartS3CleanupScheduler(ctx context.Context) {
	log.Println("🪣  Starting S3 storage cleanup scheduler (every 1 min, wall-clock aligned)...")

	runSync := func() {
		if err := SyncS3Cleanup(); err != nil {
			log.Printf("⚠️ S3 cleanup failed: %v", err)
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
			log.Println("⏹️ S3 cleanup scheduler stopped")
			return
		case <-ticker.C:
			runSync()
		}
	}
}
