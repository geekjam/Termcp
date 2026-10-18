package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/internal/securestore"
)

// StorageGateway keeps bootstrap assets and management available while the
// business graph is absent. Main wraps the entire gateway in HTTP auth.
func StorageGateway(controller *securestore.Controller, management *Handler) http.Handler {
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, status, map[string]any{"error": code, "storage": controller.Status()})
	}
	mux.HandleFunc("GET /api/storage/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, controller.Status())
	})
	passwordOperation := func(operation string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if !storageTransportAllowed(r) {
				respond(w, http.StatusUpgradeRequired, "secure_transport_required")
				return
			}
			if content := strings.Split(r.Header.Get("Content-Type"), ";")[0]; strings.TrimSpace(content) != "application/json" {
				respond(w, http.StatusUnsupportedMediaType, "json_required")
				return
			}
			var input struct {
				Password        string                 `json:"password"`
				CurrentPassword string                 `json:"current_password"`
				NewPassword     string                 `json:"new_password"`
				ConfirmPassword string                 `json:"confirm_password"`
				UnlockMode      encryptedfs.UnlockMode `json:"unlock_mode"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
				respond(w, http.StatusBadRequest, "invalid_request")
				return
			}
			password, current := []byte(input.Password), []byte(input.CurrentPassword)
			next, confirmation := []byte(input.NewPassword), []byte(input.ConfirmPassword)
			defer clear(password)
			defer clear(current)
			defer clear(next)
			defer clear(confirmation)
			var err error
			success := http.StatusAccepted
			if operation != "unlock-mode" && input.UnlockMode != "" {
				respond(w, http.StatusBadRequest, "invalid_request")
				return
			}
			switch operation {
			case "initialize":
				if input.Password != "" || input.CurrentPassword != "" {
					respond(w, http.StatusBadRequest, "invalid_request")
					return
				}
				err = controller.Initialize(next, confirmation)
			case "unlock":
				if input.CurrentPassword != "" || input.NewPassword != "" || input.ConfirmPassword != "" {
					respond(w, http.StatusBadRequest, "invalid_request")
					return
				}
				err = controller.Unlock(password)
			case "password":
				if input.Password != "" {
					respond(w, http.StatusBadRequest, "invalid_request")
					return
				}
				err = controller.ChangePassword(current, next, confirmation)
				success = http.StatusOK
			case "unlock-mode":
				if input.Password != "" {
					respond(w, http.StatusBadRequest, "invalid_request")
					return
				}
				err = controller.SetUnlockMode(input.UnlockMode, current, next, confirmation)
				success = http.StatusOK
			}
			if err != nil {
				switch {
				case errors.Is(err, encryptedfs.ErrConfirmation):
					respond(w, http.StatusBadRequest, "password_confirmation_mismatch")
				case errors.Is(err, encryptedfs.ErrAuthentication):
					code := "invalid_storage_password"
					if operation == "password" || operation == "unlock-mode" {
						code = "invalid_current_password"
					}
					respond(w, http.StatusForbidden, code)
				case errors.Is(err, securestore.ErrRateLimited):
					w.Header().Set("Retry-After", "60")
					respond(w, http.StatusTooManyRequests, "password_rate_limited")
				case errors.Is(err, securestore.ErrState):
					respond(w, http.StatusConflict, "storage_state_conflict")
				case errors.Is(err, encryptedfs.ErrDurability):
					respond(w, http.StatusServiceUnavailable, "key_save_uncertain")
				case errors.Is(err, encryptedfs.ErrProviderUnavailable):
					if operation == "unlock-mode" && input.UnlockMode == encryptedfs.UnlockAutomatic {
						respond(w, http.StatusServiceUnavailable, "automatic_unlock_unavailable")
					} else {
						respond(w, http.StatusBadRequest, "invalid_storage_password")
					}
				case errors.Is(err, encryptedfs.ErrUnlockMode):
					respond(w, http.StatusBadRequest, "invalid_unlock_mode")
				case operation == "unlock-mode" && errors.Is(err, encryptedfs.ErrFormat):
					respond(w, http.StatusBadRequest, "invalid_request")
				default:
					respond(w, http.StatusInternalServerError, "storage_operation_failed")
				}
				return
			}
			writeJSON(w, success, controller.Status())
		}
	}
	csrf := http.NewCrossOriginProtection()
	for _, operation := range []string{"initialize", "unlock", "password", "unlock-mode"} {
		mux.Handle("POST /api/storage/"+operation, csrf.Handler(passwordOperation(operation)))
	}
	mux.HandleFunc("GET /api/version", management.handleVersion)
	mux.HandleFunc("GET /api/daemon", management.handleDaemonInfo)
	mux.Handle("POST /api/daemon/stop", csrf.Handler(http.HandlerFunc(management.handleDaemonStop)))
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"storage": controller.Status().State})
	})
	assets := embeddedStaticServer()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/storage.html" && uiEmbedded {
			assets.ServeHTTP(w, r)
			return
		}
		if active := controller.Handler(); active != nil {
			active.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			path := r.URL.Path
			if path == "/" && uiEmbedded {
				w.Header().Set("Location", "storage.html")
				w.WriteHeader(http.StatusSeeOther)
				return
			}
			if path == "/api.md" || path == "/SKILL.md" || path == "/skills.md" || uiEmbedded && (path == "/api.html" || strings.HasPrefix(path, "/static/") || strings.HasPrefix(path, "/themes/") || strings.HasPrefix(path, "/icons/") || strings.HasPrefix(path, "/vendor/")) {
				assets.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Retry-After", "2")
		respond(w, http.StatusServiceUnavailable, "storage_unavailable")
	})
	return mux
}

func storageTransportAllowed(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	address := net.ParseIP(host)
	if address != nil && address.IsLoopback() {
		return true
	}
	if address == nil || !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return false
	}
	// Forwarding headers are meaningful only from explicitly trusted proxies.
	// A direct remote client cannot opt itself into plaintext password exchange.
	for _, value := range strings.Split(os.Getenv("TERMCP_STORAGE_TRUSTED_PROXIES"), ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err == nil && network.Contains(address) {
			return true
		}
	}
	return false
}
