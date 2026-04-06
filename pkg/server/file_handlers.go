package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/sahil-shubham/bhatti/pkg/agent"
)


func (s *Server) handleSandboxFiles(w http.ResponseWriter, r *http.Request, id string) {
	sb := s.getUserSandbox(w, r, id)
	if sb == nil {
		return
	}

	if err := s.ensureHot(r.Context(), sb.EngineID); err != nil {
		errResp(w, 500, "wake sandbox: "+err.Error())
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		errResp(w, 400, "path query parameter required")
		return
	}

	fe, ok := s.engine.(FileEngine)
	if !ok {
		errResp(w, 501, "engine does not support file operations")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("ls") == "true" {
			files, err := fe.FileList(r.Context(), sb.EngineID, path)
			if err != nil {
				errRespInternal(w, r, "list directory failed", err)
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
				errRespInternal(w, r, "stat file failed", err)
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
	case http.MethodPut:
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
			errRespInternal(w, r, "write file failed", err)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	case http.MethodHead:
		info, err := fe.FileStat(r.Context(), sb.EngineID, path)
		if err != nil {
			errResp(w, 404, err.Error())
			return
		}
		w.Header().Set("X-File-Size", fmt.Sprint(info.Size))
		w.Header().Set("X-File-Mode", info.Mode)
		w.Header().Set("X-File-IsDir", fmt.Sprint(info.IsDir))
		w.WriteHeader(200)
	default:
		errResp(w, 405, "method not allowed")
	}
}
