package api

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const catalogUploadLimit = 50 * 1024 * 1024

func (s *Server) writeUploadedCapture(w http.ResponseWriter, r *http.Request, id int64) {
	capture, err := scanCatalogCapture(s.db.QueryRowContext(r.Context(), `SELECT `+catalogCaptureColumns+` FROM captures c WHERE c.id=?`, id).Scan)
	if err != nil {
		s.catalogFailure(w, err)
		return
	}
	captures := []catalogCapture{capture}
	if err = s.personalize(r.Context(), captures, s.optionalCurrentUser(r)); err != nil {
		s.catalogFailure(w, err)
		return
	}
	writeJSON(w, 201, captures[0])
}

// handleCatalogUpload は画像を全体検証し、排他的な新規作成と即時登録を行う。
func (s *Server) handleCatalogUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, catalogUploadLimit+1024*1024)
	if err := r.ParseMultipartForm(8 * 1024 * 1024); err != nil {
		var large *http.MaxBytesError
		if errors.As(err, &large) {
			writeError(w, 413, "Image file is too large")
		} else {
			writeError(w, 422, "Please upload JPEG or PNG image")
		}
		return
	}
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	file, header, err := r.FormFile("image")
	if err != nil {
		writeError(w, 422, "Please upload JPEG or PNG image")
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, catalogUploadLimit+1))
	if err != nil {
		writeError(w, 422, "Please upload JPEG or PNG image")
		return
	}
	if len(data) > catalogUploadLimit {
		writeError(w, 413, "Image file is too large")
		return
	}
	modified := time.Now()
	hasModified := false
	if values, ok := r.MultipartForm.Value["file_modified_at"]; ok && len(values) > 0 {
		value, err := strconv.ParseFloat(values[len(values)-1], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			writeError(w, 422, "Invalid file_modified_at")
			return
		}
		modified, err = captureBroadcastTime(time.Unix(0, 0), value/1000)
		if err != nil {
			writeError(w, 422, "Invalid file_modified_at")
			return
		}
		hasModified = true
	}
	// Go の FileHeader.Filename はパスを取り除くため、受信したヘッダーの元の名前も検査する。
	_, params, err := mime.ParseMediaType(header.Header.Get("Content-Disposition"))
	filename := params["filename"]
	if err != nil || filename == "" || filename == "." || filename == ".." || strings.ContainsAny(filename, "/\\\x00:") {
		writeError(w, 422, "Specified filename is invalid")
		return
	}
	filename = strings.Map(func(ch rune) rune {
		if ch >= 1 && ch <= 31 || strings.ContainsRune(`<>"|?*`, ch) {
			return '_'
		}
		return ch
	}, filename)
	if !captureImagePath(filename) {
		writeError(w, 422, "Please upload JPEG or PNG image")
		return
	}
	info, err := readCatalogImage(data, filename, modified)
	if err != nil {
		writeError(w, 422, "Please upload JPEG or PNG image")
		return
	}
	c := s.captureCatalog
	var existing int64
	err = c.db.QueryRowContext(r.Context(), `SELECT id FROM captures WHERE file_hash=?`, info.Hash[:]).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.catalogFailure(w, err)
		return
	}
	if err == nil {
		files, err := c.files(r.Context(), existing)
		if err != nil {
			s.catalogFailure(w, err)
			return
		}
		for _, location := range files {
			if location.Status != "Available" {
				continue
			}
			lock := &c.locks[c.lockIndex(location.Path)]
			lock.Lock()
			unchanged, checkErr := func() (bool, error) {
				stat, err := os.Stat(location.Path)
				if err != nil {
					return false, nil
				}
				created, modified := catalogFileTimes(stat)
				if stat.Size() != location.Size || created != location.Created || modified != location.Modified {
					return false, nil
				}
				raw, err := os.ReadFile(location.Path)
				if err != nil {
					return false, nil
				}
				hash := sha256.Sum256(raw)
				if hash == info.Hash {
					return true, nil
				}
				_, err = c.registerLocked(r.Context(), location.Path, raw, true)
				return false, err
			}()
			lock.Unlock()
			if checkErr != nil {
				s.catalogFailure(w, checkErr)
				return
			}
			if unchanged {
				s.writeUploadedCapture(w, r, existing)
				return
			}
		}
	}
	for index, folder := range s.config.Capture.UploadFolders {
		folder = normalizeCapturePath(folder)
		if stat, err := os.Stat(folder); err != nil || !stat.IsDir() {
			continue
		}
		if free, err := freeDiskSpace(folder); err == nil && free < 10*1024*1024 && index < len(s.config.Capture.UploadFolders)-1 {
			continue
		}
		ext := filepath.Ext(filename)
		stem := strings.TrimSuffix(filename, ext)
		for count := 0; ; count++ {
			name := filename
			if count > 0 {
				name = fmt.Sprintf("%s-%d%s", stem, count, ext)
			}
			path := filepath.Join(folder, name)
			lock := &c.locks[c.lockIndex(path)]
			lock.Lock()
			id, saveErr := func() (int64, error) {
				output, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
				if err != nil {
					return 0, err
				}
				created := true
				defer func() {
					if created {
						_ = os.Remove(path)
					}
				}()
				_, writeErr := output.Write(data)
				closeErr := output.Close()
				if writeErr != nil {
					return 0, writeErr
				}
				if closeErr != nil {
					return 0, closeErr
				}
				if hasModified {
					if err := os.Chtimes(path, modified, modified); err != nil {
						return 0, err
					}
				}
				id, err := c.registerLocked(r.Context(), path, data, false)
				if err != nil {
					return 0, err
				}
				if id == 0 {
					return 0, fmt.Errorf("uploaded image was not registered")
				}
				created = false
				return id, nil
			}()
			lock.Unlock()
			if errors.Is(saveErr, os.ErrExist) {
				continue
			}
			if saveErr != nil {
				detail := "Unexpected error occurred while saving the file"
				if errors.Is(saveErr, os.ErrPermission) {
					detail = "Permission denied to save the file"
				} else if isDiskFullError(saveErr) {
					detail = "No space left on the device"
				}
				s.logger.Warn("Capture upload failed.", "error", saveErr)
				writeError(w, 422, detail)
				return
			}
			s.writeUploadedCapture(w, r, id)
			return
		}
	}
	writeError(w, 422, "No available folder to save the file")
}
