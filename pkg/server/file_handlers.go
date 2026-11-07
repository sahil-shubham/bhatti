package server

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// fileTarget is the prelude shared by the file handlers: it resolves the
// sandbox, wakes it, and requires the path query parameter and a FileEngine.
// On !ok the response has been written.
func (s *Server) fileTarget(w http.ResponseWriter, r *http.Request) (sb *store.Sandbox, fe FileEngine, path string, ok bool) {
	sb = s.getUserSandbox(w, r, r.PathValue("id"))
	if sb == nil {
		return nil, nil, "", false
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return nil, nil, "", false
	}

	path = r.URL.Query().Get("path")
	if path == "" {
		errResp(w, 400, "path query parameter required")
		return nil, nil, "", false
	}

	fe, ok = s.engine.(FileEngine)
	if !ok {
		errResp(w, 501, "engine does not support file operations")
		return nil, nil, "", false
	}
	return sb, fe, path, true
}

// handleSandboxFileRead serves GET /sandboxes/{id}/files: file content, or a
// directory listing with ?ls=true.
func (s *Server) handleSandboxFileRead(w http.ResponseWriter, r *http.Request) {
	sb, fe, path, ok := s.fileTarget(w, r)
	if !ok {
		return
	}

	if r.URL.Query().Get("ls") == "true" {
		files, err := fe.FileList(r.Context(), sb.EngineID, path)
		if err != nil {
			fileReadErrResp(w, r, "list directory failed", path, err)
			return
		}
		writeJSON(w, 200, files)
	} else {
		// Parse truncation parameters
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		maxBytes, _ := strconv.Atoi(r.URL.Query().Get("max_bytes"))
		truncating := limit > 0 || maxBytes > 0

		// Stat first to detect errors before writing response body.
		info, err := fe.FileStat(r.Context(), sb.EngineID, path)
		if err != nil {
			fileReadErrResp(w, r, "stat file failed", path, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// Only set Content-Length for full reads — truncated reads
		// produce an unknown final size.
		if !truncating {
			w.Header().Set("Content-Length", fmt.Sprint(info.Size))
		}
		// Report the total file size so the client knows if content
		// was truncated vs the full file.
		w.Header().Set("X-File-Size", fmt.Sprint(info.Size))
		w.WriteHeader(200)

		if truncating {
			fe.FileRead(r.Context(), sb.EngineID, path, w, agent.FileReadOpts{
				Offset:   offset,
				Limit:    limit,
				MaxBytes: maxBytes,
			})
		} else {
			fe.FileRead(r.Context(), sb.EngineID, path, w)
		}
	}
}

// handleSandboxFileWrite serves PUT /sandboxes/{id}/files.
func (s *Server) handleSandboxFileWrite(w http.ResponseWriter, r *http.Request) {
	sb, fe, path, ok := s.fileTarget(w, r)
	if !ok {
		return
	}

	size := r.ContentLength
	// Reject unknown Content-Length (chunked/missing)
	if size < 0 {
		errResp(w, 400, "Content-Length header required for file upload")
		return
	}
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "0644"
	}
	if err := fe.FileWrite(r.Context(), sb.EngineID, path, mode, size, r.Body); err != nil {
		// The agent's message ("no space: need ..., ... available") is
		// user-facing; anything else stays a logged, generic 500.
		switch {
		case errors.Is(err, agent.ErrNoSpace):
			errResp(w, http.StatusInsufficientStorage, err.Error())
		case errors.Is(err, agent.ErrFileTooLarge):
			errResp(w, http.StatusRequestEntityTooLarge, err.Error())
		default:
			errRespInternal(w, r, "write file failed", err)
		}
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// handleSandboxFileStat serves HEAD /sandboxes/{id}/files: size, mode and
// directory flag as X-File-* headers.
func (s *Server) handleSandboxFileStat(w http.ResponseWriter, r *http.Request) {
	sb, fe, path, ok := s.fileTarget(w, r)
	if !ok {
		return
	}

	info, err := fe.FileStat(r.Context(), sb.EngineID, path)
	if err != nil {
		fileReadErrResp(w, r, "stat file failed", path, err)
		return
	}
	w.Header().Set("X-File-Size", fmt.Sprint(info.Size))
	w.Header().Set("X-File-Mode", info.Mode)
	w.Header().Set("X-File-IsDir", fmt.Sprint(info.IsDir))
	w.WriteHeader(200)
}

// fileReadErrResp answers a failed read, stat or list: a missing path is the
// caller's mistake (404 naming it); anything else is a logged, generic 500.
func fileReadErrResp(w http.ResponseWriter, r *http.Request, logMsg, path string, err error) {
	if errors.Is(err, agent.ErrFileNotFound) {
		errResp(w, http.StatusNotFound, "no such file or directory: "+path)
		return
	}
	errRespInternal(w, r, logMsg, err)
}
