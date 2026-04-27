package services

import (
	"context"
	"log"
	"time"

	"server-service/internal/db/database"
	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
)

const (
	SettingBatchCapacity = "batch_capacity"
	DefaultBatchCapacity = 5
)

// GetBatchCapacity reads batch_capacity from the settings collection.
// Returns DefaultBatchCapacity (5) if the setting is missing or invalid.
func GetBatchCapacity() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var setting models.Setting
	err := database.Settings().FindOne(ctx, bson.M{"name": SettingBatchCapacity}).Decode(&setting)
	if err != nil {
		return DefaultBatchCapacity
	}

	v := setting.GetInt(DefaultBatchCapacity)
	if v <= 0 {
		return DefaultBatchCapacity
	}
	return v
}

// GetDomainContent fetches the domain_content setting from database.
func GetDomainContent(ctx context.Context) string {
	var setting models.Setting
	err := database.Settings().FindOne(ctx, bson.M{"name": "domain_content"}).Decode(&setting)
	if err != nil {
		return ""
	}
	if domainStr, ok := setting.Value.(string); ok && domainStr != "" {
		return domainStr
	}
	return ""
}

// GetDomainAsset fetches the domain_asset setting from database.
func GetDomainAsset(ctx context.Context) string {
	var setting models.Setting
	err := database.Settings().FindOne(ctx, bson.M{"name": "domain_asset"}).Decode(&setting)
	if err != nil {
		return ""
	}
	if domainStr, ok := setting.Value.(string); ok && domainStr != "" {
		return domainStr
	}
	return ""
}

// unused log helper
func logWarn(msg string, args ...interface{}) {
	log.Printf(msg, args...)
}

// ─── Hetzner Auto-Scale Setting ───────────────────────────────────────

// HetznerAutoScaleConfig holds the hetzner_auto_scale setting value.
// MongoDB document: { name: "hetzner_auto_scale", value: { ... } }
type HetznerAutoScaleConfig struct {
	Enabled               bool     `bson:"enabled"               json:"enabled"`
	APIToken              string   `bson:"apiToken"               json:"apiToken"`
	ServerType            string   `bson:"serverType"             json:"serverType"`
	Location              string   `bson:"location"               json:"location"`
	MaxServers            int      `bson:"maxServers"             json:"maxServers"`
	DownloadsPerServer    int      `bson:"downloadsPerServer"     json:"downloadsPerServer"` // legacy fallback
	// Weighted scaling: slow = missav HLS (heavy), fast = upload/direct/others (light)
	SlowPerServer         int      `bson:"slowPerServer"          json:"slowPerServer"`          // missav jobs per server (default 2)
	FastPerServer         int      `bson:"fastPerServer"          json:"fastPerServer"`          // upload/direct jobs per server (default 5)
	IdleMinutes           int      `bson:"idleMinutes"            json:"idleMinutes"`
	DeletionWindowMinutes int      `bson:"deletionWindowMinutes" json:"deletionWindowMinutes"` // minutes before billing hour to delete (default 5)
	MongoURI              string   `bson:"mongoUri"               json:"mongoUri"`
	InstallURL            string   `bson:"installUrl"             json:"installUrl"`
	StoragePath           string   `bson:"storagePath"            json:"storagePath"`
	SSHKeys               []string `bson:"sshKeys"                json:"sshKeys"`
	SSHKeyContent         string   `bson:"sshKeyContent"          json:"sshKeyContent"`
	SSHKeyPath            string   `bson:"sshKeyPath"             json:"sshKeyPath"`
}

// IsValid returns true if the config has the minimum required fields.
func (c *HetznerAutoScaleConfig) IsValid() bool {
	return c.Enabled && c.APIToken != "" && c.MongoURI != ""
}

// applyDefaults fills in zero-value fields with sensible defaults.
func (c *HetznerAutoScaleConfig) applyDefaults() {
	if c.ServerType == "" {
		c.ServerType = "cx22"
	}
	if c.Location == "" {
		c.Location = "nbg1"
	}
	if c.MaxServers <= 0 {
		c.MaxServers = 10
	}
	if c.DownloadsPerServer <= 0 {
		c.DownloadsPerServer = 2
	}
	if c.SlowPerServer <= 0 {
		c.SlowPerServer = 2 // missav: 2 jobs / server
	}
	if c.FastPerServer <= 0 {
		c.FastPerServer = 5 // upload/direct/others: 5 jobs / server
	}
	if c.IdleMinutes <= 0 {
		c.IdleMinutes = 10
	}
	if c.DeletionWindowMinutes <= 0 {
		c.DeletionWindowMinutes = 5 // delete within last 5 min of billing hour
	}
	if c.InstallURL == "" {
		c.InstallURL = "https://raw.githubusercontent.com/vdohide-core/server-download/main/install.sh"
	}
	if c.StoragePath == "" {
		c.StoragePath = "/home/files"
	}
}

// hetznerSettingDoc is used to decode the setting document with a typed value.
type hetznerSettingDoc struct {
	Value HetznerAutoScaleConfig `bson:"value"`
}

// GetHetznerAutoScale reads the hetzner_auto_scale setting from the database.
// Returns nil if the setting is missing or disabled.
func GetHetznerAutoScale(ctx context.Context) *HetznerAutoScaleConfig {
	var doc hetznerSettingDoc
	err := database.Settings().FindOne(ctx, bson.M{"name": "hetzner_auto_scale"}).Decode(&doc)
	if err != nil {
		return nil
	}
	cfg := doc.Value
	cfg.applyDefaults()
	return &cfg
}

// GetHetznerSSHKey reads the SSH private key PEM from the
// "hetzner_ssh_private_key" setting in the database.
// Returns empty string if not configured.
func GetHetznerSSHKey(ctx context.Context) string {
	var s models.Setting
	err := database.Settings().FindOne(ctx, bson.M{"name": "hetzner_ssh_private_key"}).Decode(&s)
	if err != nil {
		return ""
	}
	return s.GetString("")
}
