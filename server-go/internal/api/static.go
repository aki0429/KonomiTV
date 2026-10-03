package api

import (
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Root ハンドラーの MIME タイプ上書きテーブル。
// server/app/app.py の mimetypes.add_type() と一致させること。
var rootMIMETypes = map[string]string{
	".css":  "text/css",
	".html": "text/html",
	".ico":  "image/x-icon",
	".js":   "application/javascript",
	".json": "application/json",
	".map":  "application/json",
}

func init() {
	// /assets 以下を配信する StaticFiles 相当の挙動でも同じ MIME タイプを使う
	for extension, contentType := range rootMIMETypes {
		_ = mime.AddExtensionType(extension, contentType)
	}
}

// handleAssets は /assets/ 以下の静的なビルド済みアセットを配信する。
// Python 版の app.mount('/assets', StaticFiles(directory=CLIENT_DIR / 'assets', html=True)) 相当。
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	assetsRoot := filepath.Join(s.paths.ClientDistDir, "assets")
	relative := strings.TrimPrefix(r.URL.Path, "/assets/")
	target := filepath.Join(assetsRoot, filepath.FromSlash(path.Clean("/"+relative)))

	// ディレクトリトラバーサル対策
	if !isWithin(assetsRoot, target) {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	info, err := os.Stat(target)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	// ディレクトリの場合は index.html があればそれを返す (html=True 相当)
	if info.IsDir() {
		indexPath := filepath.Join(target, "index.html")
		indexInfo, err := os.Stat(indexPath)
		if err != nil || indexInfo.IsDir() {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		s.serveFileWithETag(w, r, indexPath, indexInfo)
		return
	}

	s.serveFileWithETag(w, r, target, info)
}

// handleRoot は /assets 以外の全パスを処理する。
// /api/ 以下は未移行分を Python 版サーバーへプロキシし、
// それ以外は client/dist の静的ファイル配信 (SPA フォールバック) を行う。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// /api/ 以下は Go 側で実装済みのルートを除き、すべて Python 版サーバーへ転送する
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if s.proxy != nil {
			s.proxy.ServeHTTP(w, r)
			return
		}
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}

	// Python 版の Root は GET のみ対応している
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	s.serveStaticClient(w, r)
}

// serveStaticClient は Python 版 app.py の Root() と同じロジックで client/dist を配信する。
func (s *Server) serveStaticClient(w http.ResponseWriter, r *http.Request) {
	root := s.paths.ClientDistDir
	relative := strings.TrimPrefix(r.URL.Path, "/")
	target := filepath.Join(root, filepath.FromSlash(relative))

	// ディレクトリトラバーサル対策
	// URL に指定されたファイルパスが CLIENT_DIR の外側を指している場合は、ファイルの有無に関わらず index.html を返す
	if !isWithin(root, target) {
		s.serveIndex(w, r, root)
		return
	}

	// ファイルが存在する場合のみそのまま配信
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		s.serveFileWithRootMIME(w, r, target, info)
		return
	}

	// ディレクトリの場合は、URL の末尾にスラッシュがついているときのみ index.html を返す
	if info, err := os.Stat(target); err == nil && info.IsDir() && (relative == "" || strings.HasSuffix(relative, "/")) {
		indexPath := filepath.Join(target, "index.html")
		if indexInfo, err := os.Stat(indexPath); err == nil && !indexInfo.IsDir() {
			s.serveFileWithRootMIME(w, r, indexPath, indexInfo)
			return
		}
	}

	// 存在しないファイルが指定された場合
	if strings.HasPrefix(relative, "api/") || strings.HasPrefix(relative, "local/") {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.serveIndex(w, r, root)
}

// serveIndex は client/dist/index.html を返す。
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request, root string) {
	indexPath := filepath.Join(root, "index.html")
	info, err := os.Stat(indexPath)
	if err != nil {
		writeError(w, http.StatusNotFound, "Not Found")
		return
	}
	s.serveFileWithRootMIME(w, r, indexPath, info)
}

// serveFileWithRootMIME は Python 版 Root() と同じ MIME タイプ判定でファイルを配信する。
// 未知の拡張子は text/plain になる点も Python 版と合わせている。
func (s *Server) serveFileWithRootMIME(w http.ResponseWriter, r *http.Request, filePath string, info fs.FileInfo) {
	contentType := rootMIMETypes[strings.ToLower(filepath.Ext(filePath))]
	if contentType == "" {
		contentType = "text/plain"
	}
	w.Header().Set("Content-Type", contentType)
	s.serveFileWithETag(w, r, filePath, info)
}

// serveFileWithETag は ETag と Last-Modified を付与してファイルを配信する。
// http.ServeFile が Range リクエストや条件付きリクエストを処理する。
func (s *Server) serveFileWithETag(w http.ResponseWriter, r *http.Request, filePath string, info fs.FileInfo) {
	etag := fmt.Sprintf("\"%x-%x\"", info.ModTime().UnixNano(), info.Size())
	w.Header().Set("ETag", etag)
	http.ServeFile(w, r, filePath)
}

// isWithin は target が root 以下に収まっているかを判定する。
func isWithin(root string, target string) bool {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	targetPath, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(rootPath, targetPath)
	if err != nil {
		return false
	}
	return relative == "." || (!strings.HasPrefix(relative, "..") && !filepath.IsAbs(relative))
}
