package api

import (
	"errors"
	"net/http"
	"sync"
)

// maintenanceVacuumState は DB を読み取らずに最適化の実行状態を返す。
type maintenanceVacuumState struct {
	mu      sync.Mutex
	running bool
}

func (s *Server) registerMaintenanceTaskRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/maintenance/capture-scan/run", s.handleMaintenanceCaptureScan)
	mux.HandleFunc("GET /api/maintenance/capture-scan/status", s.handleMaintenanceCaptureStatus)
	mux.HandleFunc("POST /api/maintenance/database-vacuum/run", s.handleMaintenanceVacuum)
	mux.HandleFunc("GET /api/maintenance/database-vacuum/status", s.handleMaintenanceVacuumStatus)
}

func (s *Server) handleMaintenanceCaptureScan(w http.ResponseWriter, r *http.Request) {
	// Go のハンドラーは切断後も終了まで動くため、処理の期限はサーバーの終了だけに結び付ける。
	complete, err := s.captureCatalog.runBatch(s.lifecycleContext)
	if errors.Is(err, errCaptureScanRunning) {
		writeError(w, 429, "Batch scan of capture folders is already running")
		return
	}
	if err != nil {
		s.logger.Error("Manual capture scan failed.", "error", err)
		writeError(w, 500, "Internal Server Error")
		return
	}
	if !complete {
		writeError(w, 500, "Some capture folders could not be scanned completely")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMaintenanceCaptureStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.captureCatalog.scanStatus())
}

func (s *Server) handleMaintenanceVacuum(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCurrentAdminUser(w, r); !ok {
		return
	}
	state := &s.vacuumState
	state.mu.Lock()
	if state.running {
		state.mu.Unlock()
		writeError(w, 429, "Database vacuum is already running")
		return
	}
	state.running = true
	state.mu.Unlock()
	defer func() { state.mu.Lock(); state.running = false; state.mu.Unlock() }()
	// 通常の書き込みと同じ単一接続を使い、ほかの書き込みを失敗させず順番に処理する。
	if _, err := s.writeDB.ExecContext(s.lifecycleContext, `VACUUM`); err != nil {
		s.logger.Error("Database vacuum failed.", "error", err)
		writeError(w, 500, "Internal Server Error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMaintenanceVacuumStatus(w http.ResponseWriter, r *http.Request) {
	s.vacuumState.mu.Lock()
	running := s.vacuumState.running
	s.vacuumState.mu.Unlock()
	writeJSON(w, 200, catalogTaskStatus{Running: running})
}
