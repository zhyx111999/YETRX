package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Sent files are immutable copies, independent of source/worktree lifetime.
// No source path is accepted by the download endpoint or exposed in metadata.
type chatFile struct {
	ID             string `json:"id"`
	ConversationID int64  `json:"conversation_id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	MIME           string `json:"mime_type"`
	SHA256         string `json:"sha256"`
}
type chatFileMessage struct {
	Text        string     `json:"text,omitempty"`
	Attachments []chatFile `json:"attachments"`
}

func validChatFileName(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, `/\`) {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}
func validChatFileID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
func (s *Server) chatFileDir(id int64) string {
	return filepath.Join(s.m.dir, "chat-files", fmt.Sprintf("conv-%d", id))
}

func (s *Server) copyChatFile(ctx context.Context, convID int64, source, name string) (file chatFile, err error) {
	if err = ctx.Err(); err != nil {
		return file, err
	}
	if name == "" {
		name = filepath.Base(source)
	}
	if !validChatFileName(name) {
		return file, fmt.Errorf("文件名无效，请使用不含目录分隔符的名称")
	}
	info, err := os.Stat(source)
	if err != nil {
		return file, fmt.Errorf("无法读取源文件: %w", err)
	}
	if !info.Mode().IsRegular() {
		return file, fmt.Errorf("只能发送普通文件；目录请先打包")
	}
	if info.Size() > maxChatUpload {
		return file, fmt.Errorf("文件超过 128 MiB 限制")
	}
	input, err := os.Open(source)
	if err != nil {
		return file, err
	}
	defer input.Close()
	info, err = input.Stat()
	if err != nil {
		return file, err
	}
	if !info.Mode().IsRegular() {
		return file, fmt.Errorf("源文件类型已改变")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return file, err
	}
	file = chatFile{ID: hex.EncodeToString(random[:]), ConversationID: convID, Name: name}
	base := s.chatFileDir(convID)
	if err = os.MkdirAll(base, 0700); err != nil {
		return file, err
	}
	dir := filepath.Join(base, file.ID)
	if err = os.Mkdir(dir, 0700); err != nil {
		return file, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
			_ = os.Remove(base)
		}
	}()
	output, err := os.OpenFile(filepath.Join(dir, "content"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return file, err
	}
	hash := sha256.New()
	file.Size, err = io.Copy(io.MultiWriter(output, hash), io.LimitReader(&chatFileReader{ctx: ctx, reader: input}, maxChatUpload+1))
	if err == nil && file.Size > maxChatUpload {
		err = fmt.Errorf("文件超过 128 MiB 限制")
	}
	if err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return file, err
	}
	if err = ctx.Err(); err != nil {
		return file, err
	}
	var sample [512]byte
	n, _ := input.ReadAt(sample[:], 0)
	file.MIME = mime.TypeByExtension(filepath.Ext(name))
	if file.MIME == "" {
		file.MIME = http.DetectContentType(sample[:n])
	}
	file.SHA256 = hex.EncodeToString(hash.Sum(nil))
	metadata, _ := json.Marshal(file)
	meta, err := os.OpenFile(filepath.Join(dir, "metadata.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return file, err
	}
	_, err = meta.Write(metadata)
	if err == nil {
		err = meta.Sync()
	}
	closeErr = meta.Close()
	if err == nil {
		err = closeErr
	}
	return file, err
}

type chatFileReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *chatFileReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *Server) removeChatFiles(id int64) {
	if err := os.RemoveAll(s.chatFileDir(id)); err != nil {
		log.Printf("[chat-files] cleanup conversation %d: %v", id, err)
	}
}

func (s *Server) downloadChatFile(w http.ResponseWriter, r *http.Request) {
	_, conv, ok := s.convByID(w, r)
	if !ok {
		return
	}
	id := r.PathValue("file")
	if !validChatFileID(id) {
		writeErr(w, http.StatusBadRequest, "无效的附件标识")
		return
	}
	root, err := os.OpenRoot(s.chatFileDir(conv.ID))
	if err != nil {
		writeErr(w, http.StatusNotFound, "附件不存在")
		return
	}
	defer root.Close()
	metadata, err := root.Open(filepath.Join(id, "metadata.json"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "附件不存在")
		return
	}
	var file chatFile
	err = json.NewDecoder(io.LimitReader(metadata, 16384)).Decode(&file)
	_ = metadata.Close()
	if err != nil || file.ID != id || file.ConversationID != conv.ID || !validChatFileName(file.Name) {
		writeErr(w, http.StatusNotFound, "附件不存在")
		return
	}
	content, err := root.Open(filepath.Join(id, "content"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "附件不存在")
		return
	}
	defer content.Close()
	info, err := content.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
		writeErr(w, http.StatusNotFound, "附件不可用")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Name}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, file.Name, info.ModTime(), content)
}
