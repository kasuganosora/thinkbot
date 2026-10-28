package channel

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// Version is the daemon build label (image label thinkbot.channel.version).
	Version = "1"
	// Protocol is the frame protocol (image label thinkbot.channel.protocol).
	Protocol = "1"

	// Magic is the 4-byte frame prefix.
	Magic      = "TBC1"
	headerSize = 16

	// DefaultMaxFrame is the maximum payload of one frame (1 MiB).
	// File and command bytes are split across frames so a put cannot pin a
	// larger buffer.
	DefaultMaxFrame = 1 << 20

	// DefaultMaxPut is the default File.Put ceiling (1000 MiB), matching the
	// user-upload cap on the main process. A smaller per-request limit still
	// applies when the client asks for one.
	DefaultMaxPut int64 = 1000 << 20

	// DefaultSock is the unix socket path inside the sandbox container.
	DefaultSock = "/run/thinkbot/channel.sock"
	// DefaultRoot is the file-operation jail (sandbox.VirtualRoot).
	DefaultRoot = "/data"

	EnvToken = "THINKBOT_CHANNEL_TOKEN"
	EnvBotID = "THINKBOT_BOT_ID"
	EnvSock  = "THINKBOT_CHANNEL_SOCK"
	EnvRoot  = "THINKBOT_CHANNEL_ROOT"
	// EnvSockUID and EnvSockGID name the peer that may dial the socket.
	// The socket stays mode 0600. Root inside the container can still connect.
	EnvSockUID = "THINKBOT_CHANNEL_SOCK_UID"
	EnvSockGID = "THINKBOT_CHANNEL_SOCK_GID"

	chunkSize = 256 << 10
)

// MsgType is the frame type. Payload is JSON except the raw data frames.
type MsgType uint16

const (
	MsgHello    MsgType = 1
	MsgHelloAck MsgType = 2
	MsgError    MsgType = 3

	MsgHealthPing       MsgType = 10
	MsgHealthPong       MsgType = 11
	MsgHealthInfo       MsgType = 12
	MsgHealthInfoResult MsgType = 13

	MsgFilePutOpen   MsgType = 20
	MsgFilePutData   MsgType = 21
	MsgFilePutCommit MsgType = 22
	MsgFilePutAbort  MsgType = 23
	MsgFilePutResult MsgType = 24

	MsgFileGetOpen MsgType = 30
	MsgFileGetData MsgType = 31
	MsgFileGetEnd  MsgType = 32

	MsgFileList       MsgType = 40
	MsgFileListResult MsgType = 41

	MsgFileDelete       MsgType = 50
	MsgFileDeleteResult MsgType = 51

	MsgFileStat       MsgType = 60
	MsgFileStatResult MsgType = 61

	MsgCommandExec         MsgType = 70
	MsgCommandStdout       MsgType = 71
	MsgCommandStderr       MsgType = 72
	MsgCommandExit         MsgType = 73
	MsgCommandSignal       MsgType = 74
	MsgCommandSignalResult MsgType = 75

	MsgProcessList         MsgType = 80
	MsgProcessListResult   MsgType = 81
	MsgProcessSignal       MsgType = 82
	MsgProcessSignalResult MsgType = 83
	MsgProcessKill         MsgType = 84

	MsgShutdown    MsgType = 90
	MsgShutdownAck MsgType = 91
)

const (
	CodeUnauthorized = "unauthorized"
	CodeBadRequest   = "bad_request"
	CodeNotFound     = "not_found"
	CodeTooLarge     = "too_large"
	CodeJail         = "jail"
	CodeIO           = "io"
	CodeBusy         = "busy"
	CodeUnsupported  = "unsupported"
)

// Capabilities is the protocol-1 surface advertised in HelloAck.
// PTY and display capture are intentionally absent.
var Capabilities = []string{
	"health.ping",
	"health.info",
	"file.put",
	"file.get",
	"file.list",
	"file.delete",
	"file.stat",
	"command.exec",
	"command.signal",
	"process.list",
	"process.signal",
	"process.kill",
	"control.shutdown",
}

// Frame is one length-prefixed message on the unix connection.
type Frame struct {
	Type    MsgType
	ReqID   uint32
	Payload []byte
}

// WriteFrame writes one frame. length covers the payload only; the header is fixed.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > DefaultMaxFrame {
		return fmt.Errorf("channel: frame payload %d exceeds %d", len(f.Payload), DefaultMaxFrame)
	}
	var hdr [headerSize]byte
	copy(hdr[0:4], Magic)
	binary.BigEndian.PutUint16(hdr[4:6], 1)
	binary.BigEndian.PutUint16(hdr[6:8], uint16(f.Type))
	binary.BigEndian.PutUint32(hdr[8:12], f.ReqID)
	binary.BigEndian.PutUint32(hdr[12:16], uint32(len(f.Payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(f.Payload) == 0 {
		return nil
	}
	_, err := w.Write(f.Payload)
	return err
}

// ReadFrame reads one frame. maxPayload <= 0 uses DefaultMaxFrame.
func ReadFrame(r io.Reader, maxPayload int) (Frame, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxFrame
	}
	var hdr [headerSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	if string(hdr[0:4]) != Magic {
		return Frame{}, errors.New("channel: bad frame magic")
	}
	if v := binary.BigEndian.Uint16(hdr[4:6]); v != 1 {
		return Frame{}, fmt.Errorf("channel: unsupported frame version %d", v)
	}
	n := binary.BigEndian.Uint32(hdr[12:16])
	if int(n) > maxPayload {
		return Frame{}, fmt.Errorf("channel: frame payload %d exceeds %d", n, maxPayload)
	}
	f := Frame{
		Type:  MsgType(binary.BigEndian.Uint16(hdr[6:8])),
		ReqID: binary.BigEndian.Uint32(hdr[8:12]),
	}
	if n == 0 {
		return f, nil
	}
	f.Payload = make([]byte, n)
	_, err := io.ReadFull(r, f.Payload)
	return f, err
}

func encode(v any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

func decode(payload []byte, v any) error {
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, v)
}

// Hello is the first authenticated frame. Token is compared in constant time
// and is never written to logs by the daemon.
type Hello struct {
	Token         string `json:"token"`
	ClientVersion string `json:"clientVersion,omitempty"`
	BotID         string `json:"botId,omitempty"`
}

// HelloAck is sent after the token (and optional bot id) match.
type HelloAck struct {
	ServerVersion string   `json:"serverVersion"`
	Protocol      string   `json:"protocol"`
	Capabilities  []string `json:"capabilities"`
	MaxPutBytes   int64    `json:"maxPutBytes"`
}

// ErrorBody is the MsgError payload.
type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RPCError is a protocol error returned to the caller.
type RPCError struct {
	Code    string
	Message string
}

func (e *RPCError) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// HealthPong is the liveness reply. It carries no secrets.
type HealthPong struct {
	OK       bool   `json:"ok"`
	Version  string `json:"version"`
	Protocol string `json:"protocol"`
}

// DiskUsage is statfs of the virtual root.
type DiskUsage struct {
	Total uint64 `json:"total"`
	Free  uint64 `json:"free"`
	Avail uint64 `json:"avail"`
}

// HealthInfo is MsgHealthInfoResult.
type HealthInfo struct {
	Version      string    `json:"version"`
	Protocol     string    `json:"protocol"`
	UptimeSec    int64     `json:"uptimeSec"`
	Root         string    `json:"root"`
	Disk         DiskUsage `json:"disk"`
	Capabilities []string  `json:"capabilities"`
}

// FileEntry matches the sandbox directory listing shape.
type FileEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime,omitempty"`
}

// PutOpen starts a streaming write. Limit 0 means "use the server ceiling".
type PutOpen struct {
	Path  string `json:"path"`
	Mkdir bool   `json:"mkdir"`
	Limit int64  `json:"limit,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
}

// PutResult is sent after commit, abort, or a limit failure.
// On CodeTooLarge the partial file is removed.
type PutResult struct {
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// GetOpen starts a streaming read. Offset/Limit are byte ranges; Limit 0 reads
// through EOF.
type GetOpen struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Limit  int64  `json:"limit,omitempty"`
}

// GetEnd finishes a stream.
type GetEnd struct {
	Bytes int64  `json:"bytes"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// ListRequest is MsgFileList.
type ListRequest struct {
	Path string `json:"path"`
}

// ListResult is MsgFileListResult.
type ListResult struct {
	Entries []FileEntry `json:"entries"`
}

// DeleteRequest is MsgFileDelete. The virtual root itself is refused.
type DeleteRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// StatRequest is MsgFileStat.
type StatRequest struct {
	Path string `json:"path"`
}

// StatResult is MsgFileStatResult.
type StatResult struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime,omitempty"`
	Mode    uint32    `json:"mode"`
	IsDir   bool      `json:"isDir"`
}

// ExecRequest runs one non-interactive command.
// Argv is executed directly when non-empty; otherwise Command is `sh -c`.
// There is no PTY, stdin stream, or resize.
type ExecRequest struct {
	Command   string   `json:"command,omitempty"`
	Argv      []string `json:"argv,omitempty"`
	Cwd       string   `json:"cwd,omitempty"`
	Env       []string `json:"env,omitempty"`
	TimeoutMs int64    `json:"timeoutMs,omitempty"`
}

// ExecResult is MsgCommandExit.
type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Signal   int    `json:"signal,omitempty"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
}

// SignalRequest names a signal ("TERM", "SIGKILL", or a number).
type SignalRequest struct {
	Signal string `json:"signal"`
	PID    int    `json:"pid,omitempty"`
}

// SignalResult acknowledges a signal.
type SignalResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Code  string `json:"code,omitempty"`
}

// Process is one row from the container process list.
type Process struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Cmd  string `json:"cmd"`
	RSS  int64  `json:"rss"`
}

// ProcessList is MsgProcessListResult.
type ProcessList struct {
	Processes []Process `json:"processes"`
}

// OkResult is a generic acknowledgement.
type OkResult struct {
	OK bool `json:"ok"`
}
