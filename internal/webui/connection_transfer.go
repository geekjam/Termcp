package webui

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func transferError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": code})
}

func (h *Handler) beginConfigTransfer(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.SSH == nil {
		transferError(w, 503, "connection_store_unavailable")
		return false
	}
	if r.URL.Query().Has("password") || r.URL.Query().Has("confirm_password") {
		transferError(w, 400, "password_in_url")
		return false
	}
	if !h.configTransferMu.TryLock() {
		w.Header().Set("Retry-After", "1")
		transferError(w, 429, "connection_transfer_busy")
		return false
	}
	return true
}

func (h *Handler) handleProtectedExportConnections(w http.ResponseWriter, r *http.Request) {
	if !h.beginConfigTransfer(w, r) {
		return
	}
	defer h.configTransferMu.Unlock()
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/json" {
		transferError(w, 415, "json_required")
		return
	}
	var input struct {
		Names        []string `json:"names"`
		Password     string   `json:"password"`
		Confirmation string   `json:"confirm_password"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || len(input.Password) > 16<<10 || len(input.Confirmation) > 16<<10 {
		transferError(w, 400, "invalid_request")
		return
	}
	password, confirmation := []byte(input.Password), []byte(input.Confirmation)
	defer clear(password)
	defer clear(confirmation)
	if subtle.ConstantTimeCompare(password, confirmation) != 1 {
		transferError(w, 400, "password_confirmation_mismatch")
		return
	}
	if len(password) != 0 && !storageTransportAllowed(r) {
		transferError(w, 426, "secure_transport_required")
		return
	}
	for _, name := range input.Names {
		if sshconfig.IsInternalName(strings.TrimSpace(name)) {
			transferError(w, 400, "reserved_profile")
			return
		}
		if sshconfig.ValidateName(name) != nil {
			transferError(w, 400, "invalid_request")
			return
		}
	}
	data, err := h.SSH.ExportBatchWithPassword(input.Names, password)
	if err != nil {
		transferError(w, 500, "connection_export_failed")
		return
	}
	defer clear(data)
	if len(password) == 0 {
		w.Header().Set("Content-Type", "application/toml; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="termcp-connections.toml"`)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="termcp-connections.toml.enc"`)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) handleProtectedImportConnections(w http.ResponseWriter, r *http.Request) {
	if !h.beginConfigTransfer(w, r) {
		return
	}
	defer h.configTransferMu.Unlock()
	temporary := false
	if raw := r.URL.Query().Get("temporary"); raw != "" {
		var err error
		temporary, err = strconv.ParseBool(raw)
		if err != nil {
			transferError(w, 400, "invalid_request")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, sshconfig.MaxTransferSize+(64<<10))
	reader, err := r.MultipartReader()
	if err != nil {
		transferError(w, 415, "multipart_required")
		return
	}
	var file, password []byte
	defer func() { clear(file); clear(password) }()
	seen := make(map[string]bool)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			transferError(w, 400, "invalid_request")
			return
		}
		name := part.FormName()
		if seen[name] || name != "file" && name != "password" {
			_ = part.Close()
			transferError(w, 400, "invalid_request")
			return
		}
		seen[name] = true
		limit := sshconfig.MaxTransferSize
		if name == "password" {
			limit = 16 << 10
		}
		data, readErr := io.ReadAll(io.LimitReader(part, int64(limit)+1))
		closeErr := part.Close()
		if readErr != nil || closeErr != nil || len(data) > limit {
			clear(data)
			transferError(w, 400, "invalid_request")
			return
		}
		if name == "file" {
			file = data
		} else {
			password = data
		}
	}
	if !seen["file"] {
		transferError(w, 400, "invalid_request")
		return
	}
	if len(password) != 0 && !storageTransportAllowed(r) {
		transferError(w, 426, "secure_transport_required")
		return
	}
	result, err := h.SSH.ImportBatchWithPassword(file, password, temporary)
	if err != nil {
		var pathError *os.PathError
		switch {
		case errors.Is(err, encryptedfs.ErrDurability):
			transferError(w, 503, "connection_import_uncertain")
		case errors.As(err, &pathError):
			transferError(w, 500, "connection_import_failed")
		case errors.Is(err, sshconfig.ErrTransferPasswordRequired):
			transferError(w, 400, "import_password_required")
		case errors.Is(err, sshconfig.ErrTransferAuthentication):
			transferError(w, 403, "invalid_import_password_or_file")
		case errors.Is(err, sshconfig.ErrTransferUnexpectedPassword):
			transferError(w, 400, "plaintext_import_password")
		default:
			transferError(w, 400, "invalid_connection_file")
		}
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
