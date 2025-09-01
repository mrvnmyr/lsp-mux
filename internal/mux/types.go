package mux

import (
	"crypto/sha1"
	"encoding/hex"
	"path/filepath"
	"time"
)

type ServerConfig struct {
	SocketPath string
	Lang       string
	RootDir    string
	Linger     time.Duration
	ServerCmd  []string
	Verbose    bool
}

func SocketPathFor(socketDir, lang, root string) string {
	sum := sha1.Sum([]byte(lang + "::" + filepath.Clean(root)))
	name := "lspmux-" + lang + "-" + hex.EncodeToString(sum[:8]) + ".sock"
	return filepath.Join(socketDir, name)
}
