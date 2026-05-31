package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"server-service/internal/db/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const workerStaleTimeout = 3 * time.Minute

// HandleWorkerList returns all workers with an isOnline flag.
// GET /workers
func (h *Handler) HandleWorkerList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	opts := options.Find().SetSort(bson.D{{Key: "hostname", Value: 1}, {Key: "workerId", Value: 1}})
	workers, err := models.WorkerModel.Find(ctx, bson.M{}, opts)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	now := time.Now()
	type workerResp struct {
		ID          string                   `json:"id"`
		WorkerID    string                   `json:"workerId"`
		Hostname    string                   `json:"hostname"`
		IP          string                   `json:"ip"`
		PID         int                      `json:"pid"`
		Enable      bool                     `json:"enable"`
		Type        string                   `json:"type"`
		Status      string                   `json:"status"`
		ActiveJobs  int                      `json:"activeJobs"`
		MaxJobs     int                      `json:"maxJobs"`
		System      *models.WorkerSystemInfo `json:"system,omitempty"`
		IsOnline    bool                     `json:"isOnline"`
		HeartbeatAt time.Time                `json:"heartbeatAt"`
		CreatedAt   time.Time                `json:"createdAt"`
	}

	out := make([]workerResp, 0, len(workers))
	for _, wr := range workers {
		isOnline := now.Sub(wr.HeartbeatAt) < workerStaleTimeout
		out = append(out, workerResp{
			ID:          wr.ID,
			WorkerID:    wr.WorkerID,
			Hostname:    wr.Hostname,
			IP:          wr.IP,
			PID:         wr.PID,
			Enable:      wr.Enable,
			Type:        wr.Type,
			Status:      wr.Status,
			ActiveJobs:  wr.ActiveJobs,
			MaxJobs:     wr.MaxJobs,
			System:      wr.System,
			IsOnline:    isOnline,
			HeartbeatAt: wr.HeartbeatAt,
			CreatedAt:   wr.CreatedAt,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"workers": out})
}
