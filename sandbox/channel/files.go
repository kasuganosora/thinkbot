package channel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

func (s *Server) effectiveLimit(req int64) int64 {
	ceiling := s.cfg.MaxPutBytes
	if ceiling == 0 {
		ceiling = DefaultMaxPut
	}
	if ceiling < 0 {
		if req > 0 {
			return req
		}
		return -1
	}
	if req <= 0 || req > ceiling {
		return ceiling
	}
	return req
}

func (c *conn) runPut(open Frame, sess *putSession) {
	defer c.endPut(open.ReqID, sess)

	var req PutOpen
	if err := decode(open.Payload, &req); err != nil {
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	full, err := JailPath(c.s.cfg.Root, req.Path)
	if err != nil {
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeJail, Error: err.Error()})
		return
	}
	if full == filepath.Clean(c.s.cfg.Root) {
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeBadRequest, Error: "refusing to write the virtual root"})
		return
	}
	parent := filepath.Dir(full)
	if req.Mkdir {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeIO, Error: err.Error()})
			return
		}
	} else if st, err := os.Stat(parent); err != nil || !st.IsDir() {
		msg := "parent directory does not exist"
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			msg = err.Error()
		}
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeNotFound, Error: msg})
		return
	}

	tmp, err := tempPath(parent)
	if err != nil {
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeIO, Error: err.Error()})
		return
	}
	f, err := openNewFile(tmp)
	if err != nil {
		c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeIO, Error: err.Error()})
		return
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(tmp)
		}
	}()

	limit := c.s.effectiveLimit(req.Limit)
	var written int64
	for {
		select {
		case <-sess.done:
			return
		case fr, ok := <-sess.ch:
			if !ok {
				return
			}
			switch fr.Type {
			case MsgFilePutData:
				if limit >= 0 && written+int64(len(fr.Payload)) > limit {
					c.sendResult(MsgFilePutResult, open.ReqID, PutResult{
						Bytes: written,
						Code:  CodeTooLarge,
						Error: ErrTooLarge.Error(),
					})
					return
				}
				n, err := f.Write(fr.Payload)
				written += int64(n)
				if err != nil {
					c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written, Code: CodeIO, Error: err.Error()})
					return
				}
			case MsgFilePutAbort:
				c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written, Code: CodeBadRequest, Error: "aborted"})
				return
			case MsgFilePutCommit:
				if err := f.Close(); err != nil {
					c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written, Code: CodeIO, Error: err.Error()})
					return
				}
				mode := os.FileMode(req.Mode)
				if mode == 0 {
					mode = 0o644
				}
				mode &= os.ModePerm
				if err := os.Chmod(tmp, mode); err != nil {
					c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written, Code: CodeIO, Error: err.Error()})
					return
				}
				if err := os.Rename(tmp, full); err != nil {
					c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written, Code: CodeIO, Error: err.Error()})
					return
				}
				committed = true
				c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Bytes: written})
				return
			default:
				c.sendResult(MsgFilePutResult, open.ReqID, PutResult{Code: CodeBadRequest, Error: "unexpected frame in put"})
				return
			}
		}
	}
}

func tempPath(dir string) (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return filepath.Join(dir, ".thinkbot-partial-"+hex.EncodeToString(buf[:])), nil
}

func (c *conn) runGet(f Frame) {
	var req GetOpen
	if err := decode(f.Payload, &req); err != nil {
		c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: CodeBadRequest, Error: err.Error()})
		return
	}
	full, err := JailPath(c.s.cfg.Root, req.Path)
	if err != nil {
		c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: CodeJail, Error: err.Error()})
		return
	}
	file, err := openFileNoFollow(c.s.cfg.Root, full)
	if err != nil {
		c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: errorCode(err), Error: err.Error()})
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: CodeIO, Error: err.Error()})
		return
	}
	if st.IsDir() {
		c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: CodeBadRequest, Error: "path is a directory"})
		return
	}
	if req.Offset > 0 {
		if _, err := file.Seek(req.Offset, io.SeekStart); err != nil {
			c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Code: CodeIO, Error: err.Error()})
			return
		}
	}
	buf := make([]byte, chunkSize)
	var sent int64
	for {
		if req.Limit > 0 && sent >= req.Limit {
			break
		}
		toRead := len(buf)
		if req.Limit > 0 && int64(toRead) > req.Limit-sent {
			toRead = int(req.Limit - sent)
		}
		n, err := file.Read(buf[:toRead])
		if n > 0 {
			if err := c.sendRaw(MsgFileGetData, f.ReqID, buf[:n]); err != nil {
				return
			}
			sent += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Bytes: sent, Code: CodeIO, Error: err.Error()})
			return
		}
	}
	c.sendResult(MsgFileGetEnd, f.ReqID, GetEnd{Bytes: sent})
}

func (c *conn) runList(f Frame) {
	var req ListRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendError(f.ReqID, CodeBadRequest, err.Error())
		return
	}
	full, err := JailPath(c.s.cfg.Root, req.Path)
	if err != nil {
		c.sendError(f.ReqID, CodeJail, err.Error())
		return
	}
	dir, err := openDirNoFollow(c.s.cfg.Root, full)
	if err != nil {
		code := errorCode(err)
		if errors.Is(err, errNotDir) {
			code = CodeBadRequest
		}
		c.sendError(f.ReqID, code, err.Error())
		return
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		c.sendError(f.ReqID, errorCode(err), err.Error())
		return
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileEntry{
			Name:    e.Name(),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	c.sendResult(MsgFileListResult, f.ReqID, ListResult{Entries: out})
}

func (c *conn) runDelete(f Frame) {
	var req DeleteRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendError(f.ReqID, CodeBadRequest, err.Error())
		return
	}
	full, err := JailPath(c.s.cfg.Root, req.Path)
	if err != nil {
		c.sendError(f.ReqID, CodeJail, err.Error())
		return
	}
	if full == filepath.Clean(c.s.cfg.Root) {
		c.sendError(f.ReqID, CodeBadRequest, "refusing to delete the virtual root")
		return
	}
	err = removeNoFollow(c.s.cfg.Root, full, req.Recursive)
	if err != nil {
		c.sendError(f.ReqID, errorCode(err), err.Error())
		return
	}
	c.sendResult(MsgFileDeleteResult, f.ReqID, OkResult{OK: true})
}

func (c *conn) runStat(f Frame) {
	var req StatRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendError(f.ReqID, CodeBadRequest, err.Error())
		return
	}
	full, err := JailPath(c.s.cfg.Root, req.Path)
	if err != nil {
		c.sendError(f.ReqID, CodeJail, err.Error())
		return
	}
	file, err := openFileNoFollow(c.s.cfg.Root, full)
	if err != nil {
		c.sendError(f.ReqID, errorCode(err), err.Error())
		return
	}
	info, err := file.Stat()
	_ = file.Close()
	if err != nil {
		c.sendError(f.ReqID, errorCode(err), err.Error())
		return
	}
	c.sendResult(MsgFileStatResult, f.ReqID, StatResult{
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    uint32(info.Mode().Perm()),
		IsDir:   info.IsDir(),
	})
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrJail):
		return CodeJail
	case errors.Is(err, ErrTooLarge):
		return CodeTooLarge
	case errors.Is(err, os.ErrNotExist):
		return CodeNotFound
	case errors.Is(err, errNotDir):
		return CodeBadRequest
	default:
		return CodeIO
	}
}

func (c *conn) sendResult(t MsgType, id uint32, v any) {
	payload, err := encode(v)
	if err != nil {
		c.sendError(id, CodeIO, err.Error())
		return
	}
	_ = c.sendRaw(t, id, payload)
}
