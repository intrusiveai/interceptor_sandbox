package sandboxd

import (
	"encoding/json"
	"os"
	"time"
)

// The TLS server's raw diagnostic can contain peer-supplied certificate text.
// Retain a connection failure only, never that text or a campaign observation.
type seamTLSLog struct {
	file    *os.File
	session string
}

func (l seamTLSLog) Write(raw []byte) (int, error) {
	data, _ := json.Marshal(map[string]any{"time": time.Now().UTC(), "session": l.session, "event": "failed", "category": "tls_handshake_failed"})
	_, err := l.file.Write(append(data, '\n'))
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}
