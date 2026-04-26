package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"server-service/internal/services"

	"golang.org/x/crypto/ssh"
)

// ─── Hetzner Server List ──────────────────────────────────────────────

// HandleHetznerServers returns the list of active managed Hetzner servers.
// GET /hetzner/servers
func (h *Handler) HandleHetznerServers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	cfg := services.GetHetznerAutoScale(ctx)
	if cfg == nil || cfg.APIToken == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"servers": []any{}, "error": "hetzner not configured"})
		return
	}

	servers, err := services.HetznerListServersPublic(cfg.APIToken)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"servers": servers})
}

// ─── Cloud-Init Log via SSH ───────────────────────────────────────────

// HandleHetznerLog SSHes into a managed server and returns the cloud-init log.
// GET /hetzner/log?ip=1.2.3.4
func (h *Handler) HandleHetznerLog(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	if ip == "" {
		http.Error(w, `{"error":"ip required"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	cfg := services.GetHetznerAutoScale(ctx)
	if cfg == nil || cfg.APIToken == "" {
		http.Error(w, `{"error":"hetzner not configured"}`, http.StatusServiceUnavailable)
		return
	}

	// Resolve private key bytes — priority:
	//  1. sshKeyContent in DB (PEM string stored directly)
	//  2. Embedded key (baked into binary at build time)
	//  3. sshKeyPath on disk
	keyBytes, err := resolveSSHKey(cfg)
	if err != nil {
		log.Printf("⚠️ SSH key resolve for %s: %v", ip, err)
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	output, err := sshFetchLog(ip, keyBytes)
	if err != nil {
		log.Printf("⚠️ SSH fetch log %s: %v", ip, err)
		http.Error(w, fmt.Sprintf(`{"error":"%v"}`, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(output))
}

// resolveSSHKey returns private key bytes from the best available source.
// Priority:
//  1. "hetzner_ssh_private_key" setting in MongoDB (dedicated setting doc)
//  2. sshKeyContent inside hetzner_auto_scale setting
//  3. sshKeyPath on disk (or default internal/ssh/keys)
func resolveSSHKey(cfg *services.HetznerAutoScaleConfig) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1. Dedicated setting: hetzner_ssh_private_key
	if keyPEM := services.GetHetznerSSHKey(ctx); keyPEM != "" {
		return []byte(keyPEM), nil
	}
	// 2. sshKeyContent inside hetzner_auto_scale
	if cfg.SSHKeyContent != "" {
		return []byte(cfg.SSHKeyContent), nil
	}
	// 3. Path on disk (optional override via sshKeyPath in hetzner_auto_scale)
	if cfg.SSHKeyPath != "" {
		b, err := os.ReadFile(cfg.SSHKeyPath)
		if err != nil {
			return nil, fmt.Errorf("SSH key file %s: %w", cfg.SSHKeyPath, err)
		}
		return b, nil
	}
	return nil, fmt.Errorf("SSH key not configured (set hetzner_ssh_private_key setting in DB)")
}

// normalizePEM fixes PEM keys that were stored in DB as a single line
// (spaces instead of newlines). Returns properly formatted PEM bytes.
func normalizePEM(raw string) []byte {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "\n") {
		// Already has newlines — ensure trailing newline
		return []byte(raw + "\n")
	}
	// Single-line PEM: "-----BEGIN TYPE----- BASE64DATA -----END TYPE-----"
	// Step 1: fix header/footer boundaries
	raw = strings.ReplaceAll(raw, "----- ", "-----\n")
	raw = strings.ReplaceAll(raw, " -----", "\n-----")
	// Step 2: reformat base64 body into 64-char lines
	parts := strings.SplitN(raw, "\n", 3)
	if len(parts) == 3 {
		body := strings.ReplaceAll(parts[1], " ", "")
		var lines []string
		for i := 0; i < len(body); i += 64 {
			end := i + 64
			if end > len(body) {
				end = len(body)
			}
			lines = append(lines, body[i:end])
		}
		raw = parts[0] + "\n" + strings.Join(lines, "\n") + "\n" + strings.TrimSpace(parts[2]) + "\n"
	}
	return []byte(raw)
}

// sshFetchLog connects to host via SSH and reads /var/log/cloud-init-output.log
func sshFetchLog(host string, keyBytes []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(normalizePEM(string(keyBytes)))
	if err != nil {
		return "", fmt.Errorf("parse SSH key: %w", err)
	}

	cfg := &ssh.ClientConfig{
		User:            "root",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec — internal tool only
		Timeout:         15 * time.Second,
	}

	client, err := ssh.Dial("tcp", host+":22", cfg)
	if err != nil {
		return "", fmt.Errorf("SSH dial %s: %w", host, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	out, err := session.Output("cat /var/log/cloud-init-output.log 2>&1 || echo 'Log not available yet'")
	if err != nil {
		return "", fmt.Errorf("SSH command: %w", err)
	}

	return string(out), nil
}
